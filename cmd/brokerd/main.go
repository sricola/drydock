package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"macagent/internal/broker"
	"macagent/internal/creds"
	"macagent/internal/egress"
	"macagent/internal/gateway"
)

func main() {
	cfg, err := egress.Load(env("EGRESS_CONFIG", "config/egress.yaml"))
	if err != nil {
		log.Fatalf("load egress config: %v", err)
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY must be set on the broker host")
	}

	imageRef := env("SANDBOX_IMAGE", "claude-sandbox:latest")
	network := env("MACAGENT_NETWORK", "macagent-egress")
	gwIP := env("MACAGENT_GW_IP", "192.168.66.1")
	gwPort, proxyPort := 8088, 3128
	budget := envFloat("MACAGENT_TASK_BUDGET_USD", 2.0)
	taskTimeout := 30 * time.Minute

	// The vmnet gateway IP only exists while a container is attached to the
	// network, so keep a persistent anchor up for the broker's lifetime. The
	// gateway/squid then bind that IP exclusively (never 0.0.0.0, which would
	// expose the credential gateway on the host's LAN/wifi).
	startAnchor(network, imageRef)

	// Credential gateway: real key host-only; the VM gets a bearer token.
	gw, err := gateway.New(apiKey, "https://api.anthropic.com", gateway.DefaultPrices())
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}
	gwAddr := net.JoinHostPort(gwIP, strconv.Itoa(gwPort))
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
		Cfg:        cfg,
		Creds:      provider,
		Approve:    func(kind string, _ any) bool { log.Printf("approval gate: %s -> auto-approve (MVP)", kind); return true },
		ImageRef:   imageRef,
		StageRoot:  env("STAGE_ROOT", "/tmp/broker/stage"),
		AuditRoot:  env("AUDIT_ROOT", "/tmp/broker/audit"),
		Timeout:    taskTimeout,
		Network:    network,
		GatewayIP:  gwIP,
		ProxyPort:  proxyPort,
		TaskBudget: budget,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", b.HandleTask)

	addr := env("BROKER_ADDR", "127.0.0.1:8765")
	log.Printf("brokerd listening on %s (gateway %s; squid expected on %s:%d)", addr, gwAddr, gwIP, proxyPort)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// startAnchor keeps the network's vmnet gateway interface up. Idempotent: any
// stale anchor is removed first.
func startAnchor(network, image string) {
	_ = exec.Command("container", "rm", "-f", "macagent-anchor").Run()
	cmd := exec.Command("container", "run", "-d", "--name", "macagent-anchor",
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
