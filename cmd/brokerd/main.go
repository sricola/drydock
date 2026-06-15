package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"macagent/internal/broker"
	"macagent/internal/creds"
	"macagent/internal/egress"
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

	b := &broker.Broker{
		Cfg:       cfg,
		Creds:     creds.StaticProvider{Key: apiKey},
		Approve:   func(kind string, _ any) bool { log.Printf("approval gate: %s -> auto-approve (MVP)", kind); return true },
		ImageRef:  env("SANDBOX_IMAGE", "claude-sandbox:latest"),
		StageRoot: env("STAGE_ROOT", "/tmp/broker/stage"),
		AuditRoot: env("AUDIT_ROOT", "/tmp/broker/audit"),
		Timeout:   30 * time.Minute,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", b.HandleTask)

	addr := env("BROKER_ADDR", "127.0.0.1:8765")
	log.Printf("brokerd listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
