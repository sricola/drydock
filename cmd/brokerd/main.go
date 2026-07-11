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

	"drydock/internal/audit"
	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/creds"
	"drydock/internal/egress"
	"drydock/internal/gateway"
	"drydock/internal/netfw"
	"drydock/internal/provider"
	"drydock/internal/runner"
	"drydock/internal/sharedir"
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

// fatal logs an error attr and exits 1. Replaces log.Fatalf without losing
// the "die loudly when bootstrap fails" UX. The error is wrapped in attrs
// so the JSON path produces structured output. Named fatal (not die) to avoid
// collision with cmd/drydock's printf-style die helper.
func fatal(msg string, attrs ...any) {
	slog.Error(msg, attrs...)
	os.Exit(1)
}

// supportedContainerMajor is the major version of Apple's `container` CLI
// drydock has been integration-tested against. Bumping this should be paired
// with re-running the smoke test in the README — `container`'s surface has
// already changed flag semantics inside 1.0.x (--user, readonly=).
const supportedContainerMajor = "1"

// defaultUncappedRequestCap bounds a task when no USD budget applies
// (subscription auth or a priceless openai_compat lane) and the operator hasn't
// set task_max_requests. High enough for a real agentic task's many tool-use
// turns, low enough to stop a runaway loop from draining a subscription.
const defaultUncappedRequestCap = 1000

// effectiveRequestCap returns the per-task request cap to enforce. When no USD
// budget bounds the backend (uncapped) and the operator left task_max_requests
// at 0/unlimited, it fails closed to defaultUncappedRequestCap; otherwise it
// honors the configured value (0 = unlimited when a USD budget is doing the
// bounding).
func effectiveRequestCap(uncapped bool, configured int) int {
	if uncapped && configured <= 0 {
		return defaultUncappedRequestCap
	}
	return configured
}

var containerVersionRE = regexp.MustCompile(`container CLI version (\d+)\.(\d+)\.(\d+)`)

// taskContainerRE matches a drydock task VM name exactly (task- + a 32-hex
// newID). Anchored so the orphan reaper's `container delete --force` can never
// fire on an unrelated container that merely has "task-" somewhere in its JSON.
var taskContainerRE = regexp.MustCompile(`^task-[0-9a-f]{32}$`)

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

// runCmd is the exec seam for container/pkill invocations. The default
// implementation calls exec.Command(name, args...).CombinedOutput() so
// production behaviour is identical to what the inline calls were. Tests
// replace this variable with a fake that records calls without spawning
// real processes.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

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
		fatal("load config", "path", config.DefaultPath(), "err", err)
	}

	initLogging(cfg.LogJSON)
	checkContainerVersion(cfg.StrictContainerVersion)

	// Single-instance: refuse to start (and run NO reaper) if another brokerd
	// is live, so its in-flight task's VM/stage/audit can't be clobbered.
	lf, err := acquireLock(config.LockPath())
	if err != nil {
		if errors.Is(err, errLockHeld) {
			fatal("another brokerd is already running on this host", "lock", config.LockPath())
		}
		fatal("cannot acquire brokerd lock", "lock", config.LockPath(), "err", err)
	}
	brokerdLock = lf

	// Unattended boots (launchd, post-reboot) can't rely on `drydock init`
	// having run this login session — ensure the container runtime is up.
	// Failure is loud but NOT fatal: exiting would make launchd's KeepAlive
	// loop; staying up lets tasks fail with a clear per-task error instead.
	containerRun := func(args ...string) (string, error) {
		out, err := exec.Command("container", args...).CombinedOutput()
		return string(out), err
	}
	if started, err := runner.EnsureContainerSystem(containerRun, func(msg string) { slog.Info(msg) }); err != nil {
		slog.Error("container system unavailable — tasks will fail until it is; try `container system start`", "err", err)
	} else if started {
		slog.Info("container system started")
	}

	pruneOrphanTasks(cfg.StageRoot, cfg.AuditRoot)

	// Egress allowlist: ~/.drydock/egress.yaml is preferred; share/drydock
	// is the seed template; CWD config/egress.yaml is the dev case.
	egressPath, err := findEgressConfig()
	if err != nil {
		fatal("locate egress config", "err", err)
	}
	egCfg, err := egress.Load(egressPath)
	if err != nil {
		fatal("load egress config", "path", egressPath, "err", err)
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
			fatal("resolve brokerd path for squid auth helper", "err", herr)
		}
		// squid splits `auth_param basic program` on whitespace with no shell, so
		// a space in the brokerd path would make it exec the wrong binary and
		// silently fail every proxy-auth check. Fail fast with a clear message.
		if strings.ContainsAny(self, " \t") {
			fatal("brokerd path contains whitespace, which breaks squid's auth_param helper; install brokerd at a path without spaces", "path", self)
		}
		helperCmd := fmt.Sprintf("%s __squid-authhelper %s", self, filepath.Join(cfg.SquidRunDir, "task-tokens"))
		squid, err = netfw.StartSquid(bin, proxyAddr, netfw.CompileSquidAllowlist(egCfg), cfg.SquidRunDir, helperCmd)
		if err != nil {
			fatal("squid start failed", "err", err)
		}
		slog.Info("squid listening", "addr", proxyAddr)
		confPath := filepath.Join(cfg.SquidRunDir, "squid.conf")
		squidCtl = netfw.NewSquidController(bin, confPath, cfg.SquidRunDir)
		// Roll squid's access/cache logs daily so they can't grow without bound
		// on a broker that runs for weeks. `logfile_rotate 10` in the conf caps
		// retained generations. The goroutine dies with the process on exit.
		go func(ctl *netfw.SquidController) {
			t := time.NewTicker(24 * time.Hour)
			defer t.Stop()
			for range t.C {
				if rerr := ctl.Rotate(); rerr != nil {
					slog.Warn("squid log rotate failed", "err", rerr)
				}
			}
		}(squidCtl)
	}

	// Stop squid and remove the anchor. Used both on signal and on a fatal
	// boot error.
	cleanup := func() {
		if serr := squid.Stop(); serr != nil {
			slog.Warn("squid stop failed; port 3128 may still be held", "err", serr)
		}
		_, _ = runCmd("container", "rm", "-f", "drydock-anchor")
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
		// Teardown (squid + anchor) and exit happen in main once Serve unblocks,
		// NOT here: if this goroutine ran cleanup() while main returned from
		// Serve in parallel, main could exit the process first and skip cleanup,
		// orphaning squid (it kept holding :3128 and broke the next start).
	}()

	// Credential gateway: real key host-only; the VM gets a bearer token.
	backends, err := buildBackends(cfg, fileKeys)
	if err != nil {
		// Pass the error text as the message so the boot log line is the
		// specific condition (e.g. "openai_compat.base_url is set but its
		// api_key_env (FOO) is empty"), identical to the pre-refactor die calls.
		cleanup()
		fatal(err.Error())
	}
	gw, err := gateway.New(backends...)
	if err != nil {
		cleanup()
		fatal("gateway init failed", "err", err)
	}
	if cfg.AggregateBudgetUSD > 0 {
		var apiKeyVendors []string
		for _, b := range backends {
			if cfg.AuthMode(b.Vendor.Name) != "subscription" {
				apiKeyVendors = append(apiKeyVendors, b.Vendor.Name)
			}
		}
		gw.SetAggregateCap(cfg.AggregateBudgetUSD, cfg.AggregateWindow, apiKeyVendors)
		if cfg.AggregateWindow > 0 {
			seedAggregateFromAudit(gw, cfg.AuditRoot, cfg.AggregateWindow, cfg.DefaultAgent)
		}
		// Log the count, not the names: apiKeyVendors is derived from backends
		// (which also carry the API key), so CodeQL's taint model treats it as
		// sensitive at a logging sink. The count conveys the same operational
		// signal (how many providers the cap covers) without the tainted flow.
		slog.Info("aggregate budget cap enabled",
			"usd", cfg.AggregateBudgetUSD, "window", cfg.AggregateWindow, "vendor_count", len(apiKeyVendors))
	}
	go func() {
		l := listenWhenReady(gwAddr)
		slog.Info("gateway listening", "addr", gwAddr)
		if serr := hardenedServer(gw).Serve(l); serr != nil {
			fatal("gateway serve failed", "err", serr)
		}
	}()

	providers := map[string]creds.Provider{}
	for _, b := range backends {
		budget := cfg.TaskBudgetUSD
		uncapped := false
		if cfg.AuthMode(b.Vendor.Name) == "subscription" {
			budget = math.MaxFloat64
			uncapped = true
		}
		if pp, ok := provider.ByVendor(b.Vendor.Name); ok && pp.ConfigBuilt && len(cfg.OpenAICompat.Prices) == 0 {
			budget = math.MaxFloat64
			uncapped = true
		}
		// When no USD budget bounds a backend (subscription auth, or a priceless
		// openai_compat lane), an unlimited request count is the whole runaway
		// control — and it defaults to 0 = unlimited, so a looping task could
		// drain a real subscription unbounded. Fail closed: apply a default
		// per-task request cap. The operator opts into a different bound (or
		// effectively unlimited) by setting task_max_requests explicitly.
		maxReq := effectiveRequestCap(uncapped, cfg.TaskMaxRequests)
		if maxReq != cfg.TaskMaxRequests {
			slog.Warn("uncapped USD budget: applying a default per-task request cap",
				"vendor", b.Vendor.Name, "request_cap", maxReq,
				"hint", "set task_max_requests to change or lift this bound")
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
			MaxRequests: maxReq,
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
	if v, ok := provider.VendorForAgent(cfg.DefaultAgent); ok {
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
		Cfg:                  egCfg,
		Providers:            providers,
		DefaultAgent:         cfg.DefaultAgent,
		ImageRef:             cfg.SandboxImage,
		StageRoot:            cfg.StageRoot,
		AuditRoot:            cfg.AuditRoot,
		Timeout:              cfg.TaskTimeout,
		ApprovalTimeout:      cfg.ApprovalTimeout,
		Network:              cfg.Network,
		GatewayIP:            cfg.GatewayIP,
		ProxyPort:            proxyPort,
		TaskBudget:           cfg.TaskBudgetUSD,
		MaxConcurrent:        cfg.MaxConcurrent,
		DefaultModel:         cfg.DefaultModel,
		OpenAICompatModel:    cfg.OpenAICompat.Model,
		Notify:               cfg.Notifications,
		AnthropicAuth:        cfg.AnthropicAuth,
		OpenAIAuth:           cfg.OpenAIAuth,
		PushMaxRetries:       cfg.PushMaxRetries,
		PushRetryBackoff:     cfg.PushRetryBackoff,
		PushFreshBranchTries: cfg.PushFreshBranchTries,
	}
	if squidCtl != nil {
		b.Squid = squidCtl
	}
	if cfg.AggregateBudgetUSD > 0 {
		b.AggregateExceeded = gw.AggregateExceeded
	}
	brk = b // expose to the shutdown handler
	slog.Info("config",
		"network", cfg.Network,
		"max_concurrent_tasks", cfg.MaxConcurrent,
		"task_budget_usd", cfg.TaskBudgetUSD,
		"default_model", cfg.DefaultModel)

	// Resume any tasks that were awaiting approval when the previous brokerd
	// shut down. Called after the broker is fully wired but before Serve.
	// Resumed tasks re-register as pending shortly after boot (asynchronously,
	// per task); an approve that races ahead of registration gets a retryable
	// 404, and the task is never lost because its marker persists.
	b.ResumeAwaiting(cfg.StageRoot)

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
		fatal("serve failed", "err", err)
	}
	// ErrServerClosed means the signal handler asked for a graceful shutdown.
	// Tear squid + the anchor down HERE, sequentially after Serve returns, so
	// process exit cannot race ahead of cleanup (the bug that orphaned squid).
	cleanup()
	if sockToRm != "" {
		_ = os.Remove(sockToRm)
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
// loopbackHostPort reports whether addr (host:port) binds a loopback address.
// An empty host (":port", all interfaces), a wildcard (0.0.0.0), or any routable
// IP is NOT loopback and would expose the admin routes to the sandbox VM. Fails
// closed on an unparseable address.
func loopbackHostPort(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func listen(cfg *config.Config, gwAddr, proxyAddr string) (net.Listener, string) {
	if tcpAddr := cfg.Broker.Addr; tcpAddr != "" {
		// The admin routes (/admin/approve, /deny, /kill) live on this listener.
		// A non-loopback TCP bind (the gateway IP, 0.0.0.0, a LAN IP) is reachable
		// from inside the sandbox VM over vmnet — which would let a task's agent
		// POST its own approval and defeat the human gate. Refuse it fail-closed;
		// loopback TCP is fine (the VM can't route to the host's 127.0.0.1), and
		// remote operators can SSH-forward the loopback port or the unix socket.
		if !loopbackHostPort(tcpAddr) {
			fatal("broker.addr must be a loopback address (127.0.0.1/::1) — a VM-reachable "+
				"bind would let a sandboxed agent approve its own pushes; use the unix socket "+
				"or SSH-forward a loopback port", "addr", tcpAddr)
		}
		slog.Warn("listening on TCP (loopback) — any LOCAL process that can reach this port can submit and approve tasks",
			"addr", tcpAddr)
		l, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			fatal("listen failed", "addr", tcpAddr, "err", err)
		}
		slog.Info("brokerd listening", "addr", tcpAddr, "gateway", gwAddr, "squid", proxyAddr)
		return l, ""
	}
	sock := cfg.Broker.Socket
	if sock == "" {
		sock = sockpath.Default()
	}
	if err := sockpath.EnsureParent(sock); err != nil {
		fatal("mkdir socket parent failed", "sock", sock, "err", err)
	}
	_ = os.Remove(sock) // stale socket from a previous crash

	// Atomically create the socket with no group/world bits — closes the
	// TOCTOU between bind() and the chmod() that used to live below.
	oldMask := syscall.Umask(0o077)
	l, err := net.Listen("unix", sock)
	syscall.Umask(oldMask)
	if err != nil {
		fatal("listen failed", "sock", sock, "err", err)
	}
	// Belt and braces: enforce 0600 explicitly even if umask gave us 0640.
	if err := os.Chmod(sock, 0o600); err != nil {
		fatal("chmod failed", "sock", sock, "err", err)
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
	fatal("addr never became bindable", "addr", addr, "hint", "is the anchor up?")
}

// checkContainerVersion fails closed if the `container` CLI isn't present, and
// either warns or fails (strict, via config strict_container_version /
// DRYDOCK_STRICT_CONTAINER_VERSION=1) when the major version doesn't match
// what drydock was tested against. Strict mode is for production / launchd
// deployments where silent drift is worse than a refusal to start.
func checkContainerVersion(strict bool) {
	out, err := runCmd("container", "--version")
	if err != nil {
		fatal("container CLI not runnable (apple/container required)", "err", err, "stderr", string(out))
	}
	m := containerVersionRE.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		if strict {
			fatal("strict mode: could not parse container version", "raw", strings.TrimSpace(string(out)))
		}
		slog.Warn("could not parse container --version output; proceeding",
			"raw", strings.TrimSpace(string(out)))
		return
	}
	version := fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])
	if m[1] != supportedContainerMajor {
		if strict {
			fatal("strict mode: container CLI version not supported",
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
	// Check the operator-override and user-owned file first.
	var first []string
	if env := os.Getenv("EGRESS_CONFIG"); env != "" {
		first = append(first, env)
	}
	if p := config.EgressPath(); p != "" {
		first = append(first, p)
	}
	for _, c := range first {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	// Dev: CWD/config/egress.yaml (running from the cloned repo). This check
	// must come before the share-dir candidates so a developer running brokerd
	// from the repo root gets the local config even if a share-dir copy exists
	// (e.g. when drydock is also installed via Homebrew).
	if _, err := os.Stat("config/egress.yaml"); err == nil {
		return "config/egress.yaml", nil
	}
	// Installed layouts: binary-relative and $HOMEBREW_PREFIX via the shared
	// sharedir helper. Locate tries CWD last, which we've already handled above;
	// the retry is harmless.
	if p, err := sharedir.Locate("config/egress.yaml"); err == nil {
		return p, nil
	}
	tried := append(first, "config/egress.yaml", "(share dirs via sharedir.Locate)")
	return "", fmt.Errorf("egress config not found; tried: %s", strings.Join(tried, ", "))
}

// pruneOrphanTasks reaps any task-* containers and orphan squid processes
// from a previous brokerd life. Apple `container run --rm` covers the
// happy path; brokerd crashes (SIGKILL, panic) and timeouts can leave the
// VM up. Squid is launched via cmd.Start() and lives past brokerd if
// brokerd doesn't receive a signal cleanly. Running this at boot closes
// the easy-orphan window before the new brokerd tries to bind 3128 again.
func pruneOrphanTasks(stageRoot, auditRoot string) {
	// Reap orphan task containers.
	out, err := runCmd("container", "ls", "-a", "--format", "json")
	if err != nil {
		slog.Warn("orphan prune: container ls failed", "err", err, "stderr", string(out))
	} else {
		// Names look like "task-<32hex>"; we don't parse the JSON shape (it moves
		// across container CLI versions), but we match each whitespace token
		// against the EXACT task-name grammar so a "task-" substring in an image
		// tag or label value can't get an unrelated container force-deleted.
		for _, line := range strings.Split(string(out), "\n") {
			for _, token := range strings.Fields(strings.ReplaceAll(line, `"`, " ")) {
				if taskContainerRE.MatchString(token) {
					_, _ = runCmd("container", "delete", "--force", token)
					slog.Info("orphan prune: removed container", "name", token)
				}
			}
		}
	}
	// A stale squid is reaped precisely (by its pidfile, only if actually dead)
	// in netfw.StartSquid → reapStaleSquid during squid setup. We deliberately
	// do NOT `pkill -f "squid -N -f"` here — that argv match could kill an
	// unrelated squid a developer happens to be running with those flags.

	// Reap host-side leftovers a crash skipped (the per-task defers never ran).
	// ORDER MATTERS — do not reorder: the container delete above must precede
	// the stage reap, because a VM mounts its work tree out of the stage dir;
	// reaping first could RemoveAll a path a still-terminating VM holds. The
	// boot lock (see main) guarantees no other brokerd is concurrently running
	// a task here, so every leftover provably belongs to a dead prior life.
	// Stages with a live gate marker must NOT be reaped: they belong to
	// awaiting-approval tasks that survived the prior shutdown and will be
	// resumed by ResumeAwaiting below.
	keep := map[string]bool{}
	for id := range broker.ListGateMarkers(auditRoot) {
		keep[id] = true
	}
	if n, err := stage.ReapOrphans(stageRoot, keep); err != nil {
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

// seedAggregateFromAudit primes the gateway's rolling aggregate ledger from the
// audit trail so the cap survives a brokerd restart. Only non-subscription
// tasks with a determinable vendor and a positive cost, whose trace mtime is
// within the window, are counted.
func seedAggregateFromAudit(gw *gateway.Gateway, auditRoot string, window time.Duration, defaultAgent string) {
	cutoff := time.Now().Add(-window)
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(auditRoot, e.Name())
		if audit.ReadMeta(path).Subscription {
			continue // subscription is out of scope for the USD cap
		}
		res, ok := audit.LastResult(path, info.Size())
		if !ok || res.TotalCostUSD <= 0 {
			continue
		}
		agent := audit.TaskAgent(path)
		if agent == "" {
			agent = defaultAgent
		}
		vendor, ok := provider.VendorForAgent(agent)
		if !ok {
			continue
		}
		gw.SeedAggregate(vendor, res.TotalCostUSD, info.ModTime())
	}
}

// startAnchor keeps the network's vmnet gateway interface up. Idempotent: any
// stale anchor is removed first. Uses a dedicated minimal image (drydock-anchor)
// FROM scratch + a static Go binary that sleeps — sharing the agent image
// here was a persistent-attack-surface risk if drydock-sandbox were ever
// compromised.
func startAnchor(network, image string) {
	_, _ = runCmd("container", "rm", "-f", "drydock-anchor")
	if out, err := runCmd("container", "run", "-d", "--name", "drydock-anchor",
		"--network", network, image); err != nil {
		fatal("start network anchor failed", "err", err, "stderr", string(out))
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
	fatal("gateway addr never became bindable", "addr", addr)
	return nil
}
