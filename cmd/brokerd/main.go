package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"drydock/internal/agent"
	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/gateway"
	"drydock/internal/netfw"
	"drydock/internal/provider"
	"drydock/internal/sockpath"
	"drydock/internal/stage"
)

// chooseLogHandler picks the slog handler. A TTY gets a terse text format
// (no timestamps — the terminal already shows time context); non-TTY (file
// redirect, launchd, SIEM tail) gets JSON so downstream tools can parse.
// jsonForced (config log_json / DRYDOCK_LOG_JSON=1) forces JSON even on a TTY.
func chooseLogHandler(w io.Writer, jsonForced, isTTY bool) slog.Handler {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if !jsonForced && isTTY {
		opts.ReplaceAttr = func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// initLogging sets the slog default handler from the resolved config value
// (which already folds in the DRYDOCK_LOG_JSON env override).
func initLogging(jsonForced bool) {
	fi, err := os.Stderr.Stat()
	isTTY := err == nil && (fi.Mode()&os.ModeCharDevice) != 0
	slog.SetDefault(slog.New(chooseLogHandler(os.Stderr, jsonForced, isTTY)))
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

// resolveAPIKey returns the effective key for env-var name. A non-empty
// exported value wins (so CI `export …` is unchanged); otherwise the value from
// the host-side api-keys.env store; else "". An env var set to "" deliberately
// falls through to the file rather than blanking a good stored key.
func resolveAPIKey(name string, fileKeys map[string]string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fileKeys[name]
}

// brokerdLock holds the single-instance flock for the process's whole life.
// Kept in a package var so the descriptor isn't garbage-collected/closed —
// closing it would drop the lock. It is intentionally write-only: nothing
// reads it; its mere existence keeps the fd (and thus the lock) alive.
//
//lint:ignore U1000 write-only by design — keeps the flock fd alive for the process's life
var brokerdLock *os.File

func main() {
	// Hidden subcommand: squid invokes this same binary as its basic-auth
	// helper (auth_param basic program <brokerd> __squid-authhelper <tokenfile>).
	if len(os.Args) >= 3 && os.Args[1] == "__squid-authhelper" {
		if err := runSquidAuthHelper(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "squid-authhelper:", err)
			os.Exit(1)
		}
		return
	}

	// Main config: ~/.drydock/config.yaml + env-var overrides. Missing file
	// is fine — defaults kick in. Loaded first so logging, the version check,
	// and notifications all honor the resolved config (YAML + env). A failure
	// here is reported via slog's built-in default handler — initLogging hasn't
	// run yet, but the error still reaches stderr.
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		die("load config", "path", config.DefaultPath(), "err", err)
	}

	initLogging(cfg.LogJSON)
	checkContainerVersion(cfg.StrictContainerVersion)

	// Single-instance: refuse to start (and run NO reaper) if another brokerd
	// is live, so its in-flight task's VM/stage/audit can't be clobbered.
	lf, err := acquireLock(config.LockPath())
	if err != nil {
		if errors.Is(err, errLockHeld) {
			die("another brokerd is already running on this host", "lock", config.LockPath())
		}
		die("cannot acquire brokerd lock", "lock", config.LockPath(), "err", err)
	}
	brokerdLock = lf

	pruneOrphanTasks(cfg.StageRoot, cfg.AuditRoot)

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

	fileKeys, _ := config.LoadAPIKeys(config.APIKeysPath())

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
	var squidCtl *netfw.SquidController
	if bin, ferr := netfw.FindSquid(); ferr != nil {
		slog.Warn("registry egress disabled", "err", ferr)
	} else {
		waitBindable(proxyAddr)
		self, herr := os.Executable()
		if herr != nil {
			die("resolve brokerd path for squid auth helper", "err", herr)
		}
		// squid splits `auth_param basic program` on whitespace with no shell, so
		// a space in the brokerd path would make it exec the wrong binary and
		// silently fail every proxy-auth check. Fail fast with a clear message.
		if strings.ContainsAny(self, " \t") {
			die("brokerd path contains whitespace, which breaks squid's auth_param helper; install brokerd at a path without spaces", "path", self)
		}
		helperCmd := fmt.Sprintf("%s __squid-authhelper %s", self, filepath.Join(cfg.SquidRunDir, "task-tokens"))
		squid, err = netfw.StartSquid(bin, proxyAddr, netfw.CompileSquidAllowlist(egCfg), cfg.SquidRunDir, helperCmd)
		if err != nil {
			die("squid start failed", "err", err)
		}
		slog.Info("squid listening", "addr", proxyAddr)
		confPath := filepath.Join(cfg.SquidRunDir, "squid.conf")
		squidCtl = netfw.NewSquidController(bin, confPath, cfg.SquidRunDir)
	}

	// Stop squid and remove the anchor. Used both on signal and on a fatal
	// boot error.
	cleanup := func() {
		if serr := squid.Stop(); serr != nil {
			slog.Warn("squid stop failed; port 3128 may still be held", "err", serr)
		}
		_ = exec.Command("container", "rm", "-f", "drydock-anchor").Run()
	}

	// Assigned once the broker + HTTP server exist; the signal handler reads
	// them nil-guarded so a signal during boot still tears squid/anchor down.
	var (
		brk      *broker.Broker
		srv      *http.Server
		sockToRm string
	)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down — cancelling in-flight tasks")
		if brk != nil {
			brk.CancelAll() // each task force-deletes its own VM and responds
		}
		if srv != nil {
			ctx, c := context.WithTimeout(context.Background(), 20*time.Second)
			_ = srv.Shutdown(ctx) // drain the cancelled handlers' responses
			c()
		}
		cleanup()
		if sockToRm != "" {
			_ = os.Remove(sockToRm)
		}
		os.Exit(0)
	}()

	// Credential gateway: real key host-only; the VM gets a bearer token.
	backends, err := buildBackends(cfg, fileKeys)
	if err != nil {
		// Pass the error text as the message so the boot log line is the
		// specific condition (e.g. "openai_compat.base_url is set but its
		// api_key_env (FOO) is empty"), identical to the pre-refactor die calls.
		cleanup()
		die(err.Error())
	}
	gw, err := gateway.New(backends...)
	if err != nil {
		cleanup()
		die("gateway init failed", "err", err)
	}
	go func() {
		l := listenWhenReady(gwAddr)
		slog.Info("gateway listening", "addr", gwAddr)
		if serr := hardenedServer(gw).Serve(l); serr != nil {
			die("gateway serve failed", "err", serr)
		}
	}()

	providers := map[string]creds.Provider{}
	for _, b := range backends {
		budget := cfg.TaskBudgetUSD
		if cfg.AuthMode(b.Vendor.Name) == "subscription" {
			budget = math.MaxFloat64
		}
		if b.Vendor.Name == "openai-compat" && len(cfg.OpenAICompat.Prices) == 0 {
			budget = math.MaxFloat64
		}
		p, _ := provider.ByVendor(b.Vendor.Name)
		providers[b.Vendor.Name] = &gateway.Provider{
			GW:          gw,
			Vendor:      b.Vendor.Name,
			BaseURL:     "http://" + gwAddr,
			BaseURLEnv:  p.BaseURLEnv,
			TokenEnv:    p.TokenEnv,
			Budget:      budget,
			TTL:         cfg.TaskTimeout + 5*time.Minute,
			MaxRequests: cfg.TaskMaxRequests,
		}
	}
	avail := make([]string, 0, len(providers))
	for v := range providers {
		avail = append(avail, v)
	}
	sort.Strings(avail)
	slog.Info("agents available", "vendors", avail)
	// Fail-loud at boot if the operator default points at a vendor with no
	// key: brokerd still starts (other agents may work), but every task that
	// doesn't pass --agent would be rejected with a 400, which is confusing
	// to debug after the fact.
	if v, ok := agent.Vendor(cfg.DefaultAgent); ok {
		if _, have := providers[v]; !have {
			pReg, _ := provider.ByVendor(v)
			slog.Warn("default_agent has no API key configured — tasks that don't pass --agent will be rejected",
				"default_agent", cfg.DefaultAgent, "set", pReg.APIKeyEnv)
		}
	}
	// Warn about openai_compat budget/routing misconfigurations — logged at
	// boot so operators see them before a task runs (warn, don't reject).
	for _, msg := range openAICompatWarnings(cfg.OpenAICompat) {
		slog.Warn(msg, "config", "openai_compat")
	}

	b := &broker.Broker{
		Cfg:               egCfg,
		Providers:         providers,
		DefaultAgent:      cfg.DefaultAgent,
		ImageRef:          cfg.SandboxImage,
		StageRoot:         cfg.StageRoot,
		AuditRoot:         cfg.AuditRoot,
		Timeout:           cfg.TaskTimeout,
		ApprovalTimeout:   cfg.ApprovalTimeout,
		Network:           cfg.Network,
		GatewayIP:         cfg.GatewayIP,
		ProxyPort:         proxyPort,
		TaskBudget:        cfg.TaskBudgetUSD,
		MaxConcurrent:     cfg.MaxConcurrent,
		DefaultModel:      cfg.DefaultModel,
		OpenAICompatModel: cfg.OpenAICompat.Model,
		Notify:            cfg.Notifications,
		AnthropicAuth:     cfg.AnthropicAuth,
		OpenAIAuth:        cfg.OpenAIAuth,
	}
	if squidCtl != nil {
		b.Squid = squidCtl
	}
	brk = b // expose to the shutdown handler
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

	srv = hardenedServer(mux)
	l, sock := listen(cfg, gwAddr, proxyAddr)
	sockToRm = sock
	// Blocks until the signal handler calls srv.Shutdown (clean) or the
	// listener errors.
	if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
		die("serve failed", "err", err)
	}
}

// hardenedServer wraps a handler with conservative header/idle timeouts to
// blunt slow-loris and idle-keepalive abuse. We deliberately do NOT set
// ReadTimeout/WriteTimeout: POST /tasks legitimately blocks for the whole task
// run + human approval, and the gateway streams long-lived agent responses —
// a body/response timeout would sever both. ReadHeaderTimeout bounds the time
// to send request headers; IdleTimeout reaps idle keep-alive connections.
func hardenedServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// listen creates the broker's listener: a per-uid Unix socket by default
// (0600, parent dir 0700, created atomically under a narrowed umask — no
// TOCTOU between bind and chmod), or a TCP socket when cfg.Broker.Addr is set
// (with a loud banner — any process that can reach the port can submit and
// approve tasks). Returns the listener and the socket path to remove on
// shutdown ("" for TCP).
func listen(cfg *config.Config, gwAddr, proxyAddr string) (net.Listener, string) {
	if tcpAddr := cfg.Broker.Addr; tcpAddr != "" {
		slog.Warn("listening on TCP — any process that can reach this port can submit and approve tasks",
			"addr", tcpAddr)
		l, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			die("listen failed", "addr", tcpAddr, "err", err)
		}
		slog.Info("brokerd listening", "addr", tcpAddr, "gateway", gwAddr, "squid", proxyAddr)
		return l, ""
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
	return l, sock
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
// either warns or fails (strict, via config strict_container_version /
// DRYDOCK_STRICT_CONTAINER_VERSION=1) when the major version doesn't match
// what drydock was tested against. Strict mode is for production / launchd
// deployments where silent drift is worse than a refusal to start.
func checkContainerVersion(strict bool) {
	out, err := exec.Command("container", "--version").CombinedOutput()
	if err != nil {
		die("container CLI not runnable (apple/container required)", "err", err, "stderr", string(out))
	}
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
func pruneOrphanTasks(stageRoot, auditRoot string) {
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

	// Reap host-side leftovers a crash skipped (the per-task defers never ran).
	// ORDER MATTERS — do not reorder: the container delete above must precede
	// the stage reap, because a VM mounts its work tree out of the stage dir;
	// reaping first could RemoveAll a path a still-terminating VM holds. The
	// boot lock (see main) guarantees no other brokerd is concurrently running
	// a task here, so every leftover provably belongs to a dead prior life.
	if n, err := stage.ReapOrphans(stageRoot); err != nil {
		slog.Warn("orphan prune: stage reap refused", "err", err)
	} else if n > 0 {
		slog.Info("orphan prune: reaped stage dirs", "count", n)
	}
	if n, err := broker.TerminateStuckAudits(auditRoot); err != nil {
		slog.Warn("orphan prune: audit terminate error", "err", err)
	} else if n > 0 {
		slog.Info("orphan prune: terminated stuck audit rows", "count", n)
	}
}

// startAnchor keeps the network's vmnet gateway interface up. Idempotent: any
// stale anchor is removed first. Uses a dedicated minimal image (drydock-anchor)
// FROM scratch + a static Go binary that sleeps — sharing the agent image
// here was a persistent-attack-surface risk if drydock-sandbox were ever
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
