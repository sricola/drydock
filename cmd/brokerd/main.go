package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/gateway"
	"drydock/internal/netfw"
	"drydock/internal/sockpath"
)

// initLogging sets the slog default handler. TTY gets a terse text format
// (no timestamps — the terminal already shows time context); non-TTY (file
// redirect, launchd, SIEM tail) gets JSON so downstream tools can parse.
// DRYDOCK_LOG_JSON=1 forces JSON even on a TTY.
func initLogging() {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if os.Getenv("DRYDOCK_LOG_JSON") != "1" {
		fi, err := os.Stderr.Stat()
		isTTY := err == nil && (fi.Mode()&os.ModeCharDevice) != 0
		if isTTY {
			opts.ReplaceAttr = func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
			return
		}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, opts)))
}

// die logs an error attr and exits 1. Replaces log.Fatalf without losing
// the "die loudly when bootstrap fails" UX. The error is wrapped in attrs
// so the JSON path produces structured output.
func die(msg string, attrs ...any) {
	slog.Error(msg, attrs...)
	os.Exit(1)
}

// supportedContainerMajor is the major version of Apple's `container` CLI
// drydock has been integration-tested against. Bumping this should be paired
// with re-running the smoke test in the README — `container`'s surface has
// already changed flag semantics inside 1.0.x (--user, readonly=).
const supportedContainerMajor = "1"

var containerVersionRE = regexp.MustCompile(`container CLI version (\d+)\.(\d+)\.(\d+)`)

func main() {
	initLogging()
	checkContainerVersion()
	pruneOrphanTasks()

	// Main config: ~/.drydock/config.yaml + env-var overrides. Missing file
	// is fine — defaults kick in.
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		die("load config", "path", config.DefaultPath(), "err", err)
	}

	// Egress allowlist: ~/.drydock/egress.yaml is preferred; share/drydock
	// is the seed template; CWD config/egress.yaml is the dev case.
	egressPath, err := findEgressConfig()
	if err != nil {
		die("locate egress config", "err", err)
	}
	egCfg, err := egress.Load(egressPath)
	if err != nil {
		die("load egress config", "path", egressPath, "err", err)
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		die("ANTHROPIC_API_KEY must be set on the broker host")
	}

	gwPort, proxyPort := 8088, 3128

	// The vmnet gateway IP only exists while a container is attached to the
	// network, so keep a persistent anchor up for the broker's lifetime. The
	// gateway/squid then bind that IP exclusively (never 0.0.0.0, which would
	// expose the credential gateway on the host's LAN/wifi).
	startAnchor(cfg.Network, cfg.AnchorImage)

	gwAddr := net.JoinHostPort(cfg.GatewayIP, strconv.Itoa(gwPort))
	proxyAddr := net.JoinHostPort(cfg.GatewayIP, strconv.Itoa(proxyPort))

	// Userspace squid for registry (npm/pip) egress: hostname allowlist, no TLS
	// interception. Bound to the vmnet gateway IP (wait until the anchor brings
	// the interface up). Optional: if squid isn't installed, registry egress is
	// simply unavailable — the model API still works via the gateway.
	var squid *netfw.Squid
	if bin, ferr := netfw.FindSquid(); ferr != nil {
		slog.Warn("registry egress disabled", "err", ferr)
	} else {
		waitBindable(proxyAddr)
		squid, err = netfw.StartSquid(bin, proxyAddr, netfw.CompileSquidAllowlist(egCfg), cfg.SquidRunDir)
		if err != nil {
			die("squid start failed", "err", err)
		}
		slog.Info("squid listening", "addr", proxyAddr)
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
		die("gateway init failed", "err", err)
	}
	go func() {
		l := listenWhenReady(gwAddr)
		slog.Info("gateway listening", "addr", gwAddr)
		if serr := http.Serve(l, gw); serr != nil {
			die("gateway serve failed", "err", serr)
		}
	}()

	var provider creds.Provider = &gateway.Provider{
		GW:      gw,
		BaseURL: "http://" + gwAddr,
		Budget:  cfg.TaskBudgetUSD,
		TTL:     cfg.TaskTimeout + 5*time.Minute,
	}

	b := &broker.Broker{
		Cfg:   egCfg,
		Creds: provider,
		Approve: func(kind string, _ any) bool {
			slog.Info("approval gate auto-approve (MVP)", "kind", kind)
			return true
		},
		ImageRef:      cfg.SandboxImage,
		StageRoot:     cfg.StageRoot,
		AuditRoot:     cfg.AuditRoot,
		Timeout:       cfg.TaskTimeout,
		Network:       cfg.Network,
		GatewayIP:     cfg.GatewayIP,
		ProxyPort:     proxyPort,
		TaskBudget:    cfg.TaskBudgetUSD,
		MaxConcurrent: cfg.MaxConcurrent,
		DefaultModel:  cfg.DefaultModel,
	}
	slog.Info("config",
		"network", cfg.Network,
		"max_concurrent_tasks", cfg.MaxConcurrent,
		"task_budget_usd", cfg.TaskBudgetUSD,
		"default_model", cfg.DefaultModel)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", b.HandleTask)
	mux.HandleFunc("POST /admin/approve/{id}", b.HandleApprove)
	mux.HandleFunc("POST /admin/deny/{id}", b.HandleDeny)
	mux.HandleFunc("POST /admin/kill/{id}", b.HandleKill)
	mux.HandleFunc("GET /admin/pending", b.HandlePending)
	mux.HandleFunc("GET /admin/tasks", b.HandleTasks)
	mux.HandleFunc("GET /healthz", b.HandleHealth)

	serve(mux, cfg, gwAddr, proxyAddr)
}

// serve listens on a Unix socket by default. The socket lives inside a
// per-uid parent dir at 0700; brokerd narrows the umask before listen() so
// the socket file itself is created at 0600 atomically (no TOCTOU window
// between bind and chmod). cfg.Broker.Addr ≠ "" opts into TCP with a
// loud startup banner — any process that can reach the port can submit
// and approve tasks, so it's for operator awareness, not security.
func serve(mux *http.ServeMux, cfg *config.Config, gwAddr, proxyAddr string) {
	if tcpAddr := cfg.Broker.Addr; tcpAddr != "" {
		slog.Warn("listening on TCP — any process that can reach this port can submit and approve tasks",
			"addr", tcpAddr)
		slog.Info("brokerd listening", "addr", tcpAddr, "gateway", gwAddr, "squid", proxyAddr)
		if err := http.ListenAndServe(tcpAddr, mux); err != nil {
			die("listen and serve failed", "err", err)
		}
		return
	}
	sock := cfg.Broker.Socket
	if sock == "" {
		sock = sockpath.Default()
	}
	if err := sockpath.EnsureParent(sock); err != nil {
		die("mkdir socket parent failed", "sock", sock, "err", err)
	}
	_ = os.Remove(sock) // stale socket from a previous crash

	// Atomically create the socket with no group/world bits — closes the
	// TOCTOU between bind() and the chmod() that used to live below.
	oldMask := syscall.Umask(0o077)
	l, err := net.Listen("unix", sock)
	syscall.Umask(oldMask)
	if err != nil {
		die("listen failed", "sock", sock, "err", err)
	}
	// Belt and braces: enforce 0600 explicitly even if umask gave us 0640.
	if err := os.Chmod(sock, 0o600); err != nil {
		die("chmod failed", "sock", sock, "err", err)
	}
	slog.Info("brokerd listening", "addr", "unix://"+sock, "gateway", gwAddr, "squid", proxyAddr)
	if err := http.Serve(l, mux); err != nil {
		die("serve failed", "err", err)
	}
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
	die("addr never became bindable", "addr", addr, "hint", "is the anchor up?")
}

// checkContainerVersion fails closed if the `container` CLI isn't present, and
// either warns or fails (with DRYDOCK_STRICT_CONTAINER_VERSION=1) when the
// major version doesn't match what drydock was tested against. Strict mode
// is for production / launchd deployments where silent drift is worse than
// a refusal to start.
func checkContainerVersion() {
	out, err := exec.Command("container", "--version").CombinedOutput()
	if err != nil {
		die("container CLI not runnable (apple/container required)", "err", err, "stderr", string(out))
	}
	strict := os.Getenv("DRYDOCK_STRICT_CONTAINER_VERSION") == "1"
	m := containerVersionRE.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		if strict {
			die("strict mode: could not parse container version", "raw", strings.TrimSpace(string(out)))
		}
		slog.Warn("could not parse container --version output; proceeding",
			"raw", strings.TrimSpace(string(out)))
		return
	}
	version := fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])
	if m[1] != supportedContainerMajor {
		if strict {
			die("strict mode: container CLI version not supported",
				"version", version, "tested", supportedContainerMajor+".x")
		}
		slog.Warn("container CLI version not in tested range — re-run README smoke test",
			"version", version, "tested", supportedContainerMajor+".x")
		return
	}
	slog.Info("container CLI", "version", version, "supported", true)
}

// findEgressConfig locates the egress allowlist YAML. ~/.drydock/egress.yaml is
// the canonical operator-owned file (seeded by `drydock init` from the share/
// template). Search order:
//  1. $EGRESS_CONFIG                              (explicit operator override)
//  2. ~/.drydock/egress.yaml                      (the user-owned file)
//  3. ./config/egress.yaml                        (dev: cloned repo)
//  4. <brokerd>/../share/drydock/config/egress.yaml  (brew + make install seed)
//  5. $HOMEBREW_PREFIX/share/drydock/config/egress.yaml
func findEgressConfig() (string, error) {
	candidates := []string{}
	if env := os.Getenv("EGRESS_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	if p := config.EgressPath(); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "config/egress.yaml")
	if self, err := os.Executable(); err == nil {
		root := filepath.Dir(filepath.Dir(self))
		candidates = append(candidates,
			filepath.Join(root, "share", "drydock", "config", "egress.yaml"))
	}
	if hb := os.Getenv("HOMEBREW_PREFIX"); hb != "" {
		candidates = append(candidates,
			filepath.Join(hb, "share", "drydock", "config", "egress.yaml"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("egress config not found; tried: %s",
		strings.Join(candidates, ", "))
}

// pruneOrphanTasks reaps any task-* containers and orphan squid processes
// from a previous brokerd life. Apple `container run --rm` covers the
// happy path; brokerd crashes (SIGKILL, panic) and timeouts can leave the
// VM up. Squid is launched via cmd.Start() and lives past brokerd if
// brokerd doesn't receive a signal cleanly. Running this at boot closes
// the easy-orphan window before the new brokerd tries to bind 3128 again.
func pruneOrphanTasks() {
	// Reap orphan task containers.
	out, err := exec.Command("container", "ls", "-a", "--format", "json").CombinedOutput()
	if err != nil {
		slog.Warn("orphan prune: container ls failed", "err", err, "stderr", string(out))
	} else {
		// Names look like "task-<hex>"; we don't bother parsing the JSON
		// shape (which moves across container CLI versions). A substring
		// is enough and won't match drydock-anchor (handled by startAnchor).
		for _, line := range strings.Split(string(out), "\n") {
			for _, token := range strings.Fields(strings.ReplaceAll(line, `"`, " ")) {
				if strings.HasPrefix(token, "task-") && len(token) > 5 {
					_ = exec.Command("container", "delete", "--force", token).Run()
					slog.Info("orphan prune: removed container", "name", token)
				}
			}
		}
	}
	// Reap orphan squid (very specific argv: "-N -f" only used by drydock).
	_ = exec.Command("pkill", "-f", "squid -N -f").Run()
}

// startAnchor keeps the network's vmnet gateway interface up. Idempotent: any
// stale anchor is removed first. Uses a dedicated minimal image (drydock-anchor)
// FROM scratch + a static Go binary that sleeps — sharing the agent image
// here was a persistent-attack-surface risk if claude-sandbox were ever
// compromised.
func startAnchor(network, image string) {
	_ = exec.Command("container", "rm", "-f", "drydock-anchor").Run()
	cmd := exec.Command("container", "run", "-d", "--name", "drydock-anchor",
		"--network", network, image)
	if out, err := cmd.CombinedOutput(); err != nil {
		die("start network anchor failed", "err", err, "stderr", string(out))
	}
	slog.Info("network anchor up", "network", network)
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
	die("gateway addr never became bindable", "addr", addr)
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
