package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"drydock/internal/broker"
	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/gateway"
	"drydock/internal/netfw"
)

// supportedContainerMajor is the major version of Apple's `container` CLI
// drydock has been integration-tested against. Bumping this should be paired
// with re-running the smoke test in the README — `container`'s surface has
// already changed flag semantics inside 1.0.x (--user, readonly=).
const supportedContainerMajor = "1"

var containerVersionRE = regexp.MustCompile(`container CLI version (\d+)\.(\d+)\.(\d+)`)

func main() {
	checkContainerVersion()

	cfg, err := egress.Load(env("EGRESS_CONFIG", "config/egress.yaml"))
	if err != nil {
		log.Fatalf("load egress config: %v", err)
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY must be set on the broker host")
	}

	imageRef := env("SANDBOX_IMAGE", "claude-sandbox:latest")
	network := env("DRYDOCK_NETWORK", "drydock-egress")
	gwIP := env("DRYDOCK_GW_IP", "192.168.66.1")
	gwPort, proxyPort := 8088, 3128
	budget := envFloat("DRYDOCK_TASK_BUDGET_USD", 2.0)
	taskTimeout := 30 * time.Minute

	// The vmnet gateway IP only exists while a container is attached to the
	// network, so keep a persistent anchor up for the broker's lifetime. The
	// gateway/squid then bind that IP exclusively (never 0.0.0.0, which would
	// expose the credential gateway on the host's LAN/wifi).
	startAnchor(network, imageRef)

	gwAddr := net.JoinHostPort(gwIP, strconv.Itoa(gwPort))
	proxyAddr := net.JoinHostPort(gwIP, strconv.Itoa(proxyPort))

	// Userspace squid for registry (npm/pip) egress: hostname allowlist, no TLS
	// interception. Bound to the vmnet gateway IP (wait until the anchor brings
	// the interface up). Optional: if squid isn't installed, registry egress is
	// simply unavailable — the model API still works via the gateway.
	var squid *netfw.Squid
	if bin, ferr := netfw.FindSquid(); ferr != nil {
		log.Printf("WARNING: %v — registry (npm/pip) egress disabled", ferr)
	} else {
		waitBindable(proxyAddr)
		squid, err = netfw.StartSquid(bin, proxyAddr, netfw.CompileSquidAllowlist(cfg), env("SQUID_RUN_DIR", "/tmp/broker/squid"))
		if err != nil {
			log.Fatalf("squid: %v", err)
		}
		log.Printf("squid listening on %s", proxyAddr)
	}

	// Graceful shutdown: stop squid and remove the anchor.
	cleanup := func() {
		_ = squid.Stop()
		_ = exec.Command("container", "rm", "-f", "drydock-anchor").Run()
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cleanup(); os.Exit(0) }()

	// Credential gateway: real key host-only; the VM gets a bearer token.
	gw, err := gateway.New(apiKey, "https://api.anthropic.com", gateway.DefaultPrices())
	if err != nil {
		cleanup()
		log.Fatalf("gateway: %v", err)
	}
	go func() {
		l := listenWhenReady(gwAddr)
		log.Printf("gateway listening on %s", gwAddr)
		log.Fatal(http.Serve(l, gw))
	}()

	var provider creds.Provider = &gateway.Provider{
		GW:      gw,
		BaseURL: "http://" + gwAddr,
		Budget:  budget,
		TTL:     taskTimeout + 5*time.Minute,
	}

	b := &broker.Broker{
		Cfg:           cfg,
		Creds:         provider,
		Approve:       func(kind string, _ any) bool { log.Printf("approval gate: %s -> auto-approve (MVP)", kind); return true },
		ImageRef:      imageRef,
		StageRoot:     env("STAGE_ROOT", "/tmp/broker/stage"),
		AuditRoot:     env("AUDIT_ROOT", "/tmp/broker/audit"),
		Timeout:       taskTimeout,
		Network:       network,
		GatewayIP:     gwIP,
		ProxyPort:     proxyPort,
		TaskBudget:    budget,
		MaxConcurrent: envInt("DRYDOCK_MAX_CONCURRENT_TASKS", 2),
	}
	log.Printf("max concurrent tasks: %d", b.MaxConcurrent)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", b.HandleTask)
	mux.HandleFunc("POST /admin/approve/{id}", b.HandleApprove)
	mux.HandleFunc("POST /admin/deny/{id}", b.HandleDeny)
	mux.HandleFunc("GET /admin/pending", b.HandlePending)
	mux.HandleFunc("GET /admin/tasks", b.HandleTasks)
	mux.HandleFunc("GET /healthz", b.HandleHealth)

	serve(mux, gwAddr, proxyAddr)
}

// serve listens on a Unix socket by default (file mode 0600, so only the
// invoking user can talk to brokerd). Setting BROKER_ADDR=host:port opts
// into TCP with a loud startup banner — any process that can reach the
// port can ship a PR, so this is for operator awareness, not security.
func serve(mux *http.ServeMux, gwAddr, proxyAddr string) {
	if tcpAddr := os.Getenv("BROKER_ADDR"); tcpAddr != "" {
		log.Printf("WARNING: BROKER_ADDR=%s — listening on TCP. Any process that can reach this port can submit/approve tasks.", tcpAddr)
		log.Printf("brokerd listening on %s (gateway %s, squid %s)", tcpAddr, gwAddr, proxyAddr)
		log.Fatal(http.ListenAndServe(tcpAddr, mux))
	}
	sock := env("BROKER_SOCKET", "/tmp/drydock.sock")
	_ = os.Remove(sock) // stale socket from a previous crash
	l, err := net.Listen("unix", sock)
	if err != nil {
		log.Fatalf("listen %s: %v", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		log.Fatalf("chmod %s: %v", sock, err)
	}
	log.Printf("brokerd listening on unix://%s (gateway %s, squid %s)", sock, gwAddr, proxyAddr)
	log.Fatal(http.Serve(l, mux))
}

// waitBindable blocks until addr can be bound (the anchor brings the vmnet
// interface up asynchronously), then releases it for the real listener.
func waitBindable(addr string) {
	for i := 0; i < 60; i++ {
		if l, err := net.Listen("tcp", addr); err == nil {
			l.Close()
			return
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("%s never became bindable (anchor up?)", addr)
}

// checkContainerVersion fails closed if the `container` CLI isn't present, and
// warns (without exiting) when the major version doesn't match what drydock
// was tested against. We choose warn-don't-fail so a verified user can bypass
// after re-running the smoke test against a newer release.
func checkContainerVersion() {
	out, err := exec.Command("container", "--version").CombinedOutput()
	if err != nil {
		log.Fatalf("container CLI not runnable (apple/container required): %v\n%s", err, out)
	}
	m := containerVersionRE.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		log.Printf("WARNING: could not parse container --version output (%q); proceeding", strings.TrimSpace(string(out)))
		return
	}
	if m[1] != supportedContainerMajor {
		log.Printf("WARNING: container CLI v%s.%s.%s — drydock is tested against v%s.x. Re-run the README smoke test before relying on this.",
			m[1], m[2], m[3], supportedContainerMajor)
		return
	}
	log.Printf("container CLI v%s.%s.%s (supported)", m[1], m[2], m[3])
}

// startAnchor keeps the network's vmnet gateway interface up. Idempotent: any
// stale anchor is removed first.
func startAnchor(network, image string) {
	_ = exec.Command("container", "rm", "-f", "drydock-anchor").Run()
	cmd := exec.Command("container", "run", "-d", "--name", "drydock-anchor",
		"--network", network, "--entrypoint", "/bin/sh", image, "-c", "sleep infinity")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("start network anchor: %v\n%s", err, out)
	}
	log.Printf("network anchor up on %s", network)
}

// listenWhenReady retries binding addr until the vmnet gateway interface comes
// up (the anchor brings it up asynchronously).
func listenWhenReady(addr string) net.Listener {
	for i := 0; i < 60; i++ {
		if l, err := net.Listen("tcp", addr); err == nil {
			return l
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("gateway: %s never became bindable (anchor up?)", addr)
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
