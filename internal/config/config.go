// Package config is the operator-facing config for drydock. It consolidates
// the env-var scatter into one YAML at ~/.drydock/config.yaml (companion to
// ~/.drydock/egress.yaml). Env vars still override file values, so existing
// scripts that set DRYDOCK_* keep working.
//
// Resolution order for every field:
//  1. The env var (DRYDOCK_NETWORK, DRYDOCK_GW_IP, etc.)
//  2. ~/.drydock/config.yaml (or the path passed to Load)
//  3. The struct default (Defaults()).
//
// ANTHROPIC_API_KEY is intentionally not in this struct — it never goes
// to disk by design.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"drydock/internal/globmatch"
	"drydock/internal/provider"
	"drydock/internal/repokey"
	"gopkg.in/yaml.v3"
)

// OpenAICompatPrice holds the USD/1M-token prices for a single model in the
// openai_compat lane. Both fields are optional; omit to skip USD metering for
// that model.
type OpenAICompatPrice struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

// OpenAICompatConfig is the operator-facing config block for the bring-your-own
// OpenAI-compatible upstream. Empty BaseURL disables the lane entirely.
type OpenAICompatConfig struct {
	BaseURL   string                       `yaml:"base_url"`
	BasePath  string                       `yaml:"base_path"`
	APIKeyEnv string                       `yaml:"api_key_env"`
	Model     string                       `yaml:"model"`
	Prices    map[string]OpenAICompatPrice `yaml:"prices"`
}

// VerifyRepo is one repository's verification recipe. Timeout 0 uses the
// built-in default (broker.DefaultVerifyTimeout). Required=true blocks the
// push unless verification status is exactly "passed" (fail-closed —
// inconclusive evidence blocks too).
type VerifyRepo struct {
	Commands [][]string    `yaml:"commands"`
	Timeout  time.Duration `yaml:"timeout"`
	Required bool          `yaml:"required"`
}

// SetupProfile is one repository's execution profile: host-approved setup
// commands run in the task VM against the staged tree before the agent
// starts, then readiness commands gate the run — any failure fails the task
// closed before any model spend. Commands are self-contained argv vectors:
// shell state (cd, exports) does not carry between commands. Timeout 0 uses
// the built-in default.
type SetupProfile struct {
	Setup     [][]string    `yaml:"setup"`
	Readiness [][]string    `yaml:"readiness"`
	Timeout   time.Duration `yaml:"timeout"`
	// Cache opts this repo into the persistent dependency cache under
	// CacheRoot: setup commands can populate a per-repo cache dir that
	// survives across tasks and is read-only to the agent. Off (the zero
	// value) keeps today's behavior — setup runs from scratch every task.
	Cache bool `yaml:"cache"`
}

// DiffPolicy is the host-side policy applied to the diff a task proposes.
// Caps and blocked paths fail the task closed as policy_blocked before it
// reaches review; second-look paths still reach review but the approver must
// acknowledge each flagged category. Path patterns are **-aware repo-relative
// globs (internal/globmatch): use "dir/**" for everything under dir — a
// trailing-slash pattern ("dir/") matches nothing and is a config error.
// The zero value disables every check.
type DiffPolicy struct {
	// MaxFilesChanged fails the task when the diff changes more than this
	// many files. 0 = no cap.
	MaxFilesChanged int `yaml:"max_files_changed"`
	// MaxLinesChanged fails the task when added+deleted lines exceed this.
	// 0 = no cap.
	MaxLinesChanged int `yaml:"max_lines_changed"`
	// BlockedPaths: touching a matching path fails the task as policy_blocked.
	BlockedPaths []string `yaml:"blocked_paths"`
	// SecondLookPaths: touching a matching path flags the diff for explicit
	// approver acknowledgement.
	SecondLookPaths []string `yaml:"second_look_paths"`
}

// CI configures host-side observation of a pushed PR's continuous integration.
// The whole block is opt-in: with Watch false (the default) brokerd starts no
// watch goroutine, writes no <id>.ci.json marker, and makes no `gh` call on a
// timer — a stock install behaves exactly as it did before the block existed.
//
// What the watch does: after a task's branch is pushed and its PR is open, the
// broker polls that PR's check CONCLUSIONS on the host (`gh pr checks` /
// `gh pr view --json statusCheckRollup`, read-only, host-pinned to github.com,
// with the same curated env every other host CLI call uses) until they resolve
// or the deadline expires. No CI log text is fetched, stored, parsed, or
// displayed, and nothing a repository's workflow prints can influence a
// decision. See THREAT_MODEL.md N5: enabling this puts the host's `gh`
// credential on a timer, where it was previously only operator-initiated.
type CIConfig struct {
	// Watch enables the host-side CI watch. Default false (off).
	Watch bool `yaml:"watch"`
	// PollInterval is the watch tick. 0 uses the broker's built-in default
	// (DefaultCIPollInterval). Every tick costs one `gh` API call per PR
	// still being watched, so this is also the rate at which the operator's
	// GitHub credential is exercised.
	PollInterval time.Duration `yaml:"poll_interval"`
	// WatchTimeout bounds a single PR's watch as an ABSOLUTE deadline
	// anchored to when the marker was written, so a restart cannot extend it.
	// A watch that expires with no conclusion is recorded honestly (the queue
	// item dead-letters); it never reads as a pass. 0 uses the broker's
	// built-in default (DefaultCIWatchTimeout).
	WatchTimeout time.Duration `yaml:"watch_timeout"`
	// MaxAttempts bounds the bounded-retry chain: on an observed CI failure
	// the broker may enqueue up to this many fresh retry tasks. 0 (the
	// default) disables retry entirely. Declared here in B1 so the schema is
	// stable; the behavior lands in B2. Each attempt mints a fresh full
	// task_budget_usd, so the worst-case spend for one chain is
	// max_attempts × task_budget_usd — which is why MaxCIAttempts caps it.
	MaxAttempts int `yaml:"max_attempts"`
}

// Config is the operator surface. yaml tags match what's written to
// ~/.drydock/config.yaml; the env-var names are documented in README.
type Config struct {
	// Container runtime
	Network      string `yaml:"network"`
	GatewayIP    string `yaml:"gateway_ip"`
	SandboxImage string `yaml:"sandbox_image"`
	AnchorImage  string `yaml:"anchor_image"`

	// Per-task limits
	TaskBudgetUSD float64       `yaml:"task_budget_usd"`
	MaxConcurrent int           `yaml:"max_concurrent_tasks"`
	TaskTimeout   time.Duration `yaml:"task_timeout"`
	// ApprovalTimeout bounds how long a task may sit at a human-approval gate
	// (diff push or egress widening) before it is auto-denied and its
	// concurrency slot is released. 0 (the default) waits indefinitely — right
	// for interactive use; set a value for unattended/batch runs so a forgotten
	// approval can't pin a slot forever.
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`

	// DefaultModel is the model passed to Claude Code and Codex tasks that don't
	// supply --model themselves. It does NOT apply to gemini (uses its own
	// default) or opencode (uses openai_compat.model). Empty = the agent picks.
	DefaultModel string `yaml:"default_model"`

	// DefaultAgent selects the sandbox CLI when a task doesn't pass --agent.
	// "claude", "codex", "gemini", or "opencode".
	DefaultAgent string `yaml:"default_agent"`

	// AnthropicAuth selects authentication mode: "api_key" or "subscription".
	AnthropicAuth string `yaml:"anthropic_auth"`

	// OpenAIAuth selects authentication mode: "api_key" or "subscription".
	OpenAIAuth string `yaml:"openai_auth"`

	// TaskMaxRequests is a per-task request cap. 0 falls closed to a built-in
	// default (broker.DefaultUncappedRequestCap) in every auth mode; set
	// explicitly to change the bound.
	TaskMaxRequests int `yaml:"task_max_requests"`

	// TaskMaxInFlight caps concurrently admitted gateway requests per task
	// lease. Spend is metered post-hoc, so each concurrently admitted request
	// can overshoot the budget by its own cost; this bounds the overshoot to
	// task_max_inflight requests. 1 (the default) restores the documented
	// "at most one request" bound. 0 = unlimited (pre-v0.6.3 behavior).
	TaskMaxInFlight int `yaml:"task_max_inflight"`

	// StageQuotaGB is the hard per-task disk bound in GiB. On macOS each
	// task's stage dir is an APFS sparse image of this size mounted at the
	// stage root, so a hostile in-VM agent writing to /work hits a
	// filesystem wall instead of the host disk (F-04). The polling stage
	// guard (4 GiB soft) fires first in normal operation; this is the
	// backstop it cannot provide. 0 disables the image (plain host dir,
	// polling guard only). Ignored off macOS.
	StageQuotaGB int `yaml:"stage_quota_gb"`

	// CacheQuotaGB is the total disk bound in GiB for the persistent
	// dependency cache under CacheRoot (all opted-in repos combined).
	// 0 disables the cache entirely, even for repos whose profile sets
	// cache: true.
	CacheQuotaGB int `yaml:"cache_quota_gb"`

	// MaxRequestCostUSD is the worst-case USD a single request may cost,
	// reserved against the lease budget while the request is in flight so
	// concurrent requests cannot all admit at spend=0. 0 (default) disables the
	// reservation (post-hoc metering only).
	MaxRequestCostUSD float64 `yaml:"max_request_cost_usd"`

	// AggregateBudgetUSD caps cross-task USD spend per api_key-mode provider over
	// AggregateWindow. 0 (default) disables the aggregate cap. Subscription
	// vendors are out of scope (bounded per-task by TaskMaxRequests).
	AggregateBudgetUSD float64 `yaml:"aggregate_budget_usd"`
	// AggregateWindow is the rolling window for AggregateBudgetUSD. 0 means a
	// total since brokerd boot (session cap, no time decay, resets on restart).
	AggregateWindow time.Duration `yaml:"aggregate_window"`

	// PushMaxRetries retries a transient (network) push failure this many times
	// with exponential backoff. 0 disables transient retry.
	PushMaxRetries int `yaml:"push_max_retries"`
	// PushRetryBackoff is the base delay for push retry backoff (base << n).
	PushRetryBackoff time.Duration `yaml:"push_retry_backoff"`
	// PushFreshBranchTries retries a branch-name collision against this many
	// alternate remote branch names (agent/<id>-2, -3, ...). 0 disables it.
	PushFreshBranchTries int `yaml:"push_fresh_branch_tries"`

	// OpenAICompat configures a bring-your-own OpenAI-compatible upstream
	// (Gemini's /v1beta/openai, OpenRouter, local). Empty BaseURL = disabled.
	// The real key is read from the host env var named by APIKeyEnv — never
	// stored here. Prices (USD per 1M tokens) enable USD metering; omit to fall
	// back to the task_max_requests cap.
	OpenAICompat OpenAICompatConfig `yaml:"openai_compat"`

	// Verify configures the independent post-run verifier: host-approved
	// commands run against the agent's staged tree in a fresh, credential-
	// free, network-denied VM before the approval gate. Keys are canonical
	// "host/owner/repo" (repokey.Normalize); non-canonical keys are a config
	// error rather than a silent never-match. Empty map = verifier off.
	Verify struct {
		Repos map[string]VerifyRepo `yaml:"repos"`
	} `yaml:"verify"`

	// Profiles configures host-side execution profiles: per-repo setup and
	// readiness commands run in the task VM before the agent starts. Host
	// config only — the sandboxed agent cannot edit them. Keys are canonical
	// "host/owner/repo" (repokey.Normalize); non-canonical keys are a config
	// error rather than a silent never-match. Empty map = disabled.
	Profiles struct {
		Repos map[string]SetupProfile `yaml:"repos"`
	} `yaml:"profiles"`

	// DiffPolicy caps the size and shape of the diff a task may propose.
	// Zero value = disabled (no caps, no blocked/second-look paths).
	DiffPolicy DiffPolicy `yaml:"diff_policy"`

	// CI configures host-side observation of a pushed PR's CI. Off by
	// default; see CIConfig.
	CI CIConfig `yaml:"ci"`

	// Where state lives
	StageRoot   string `yaml:"stage_root"`
	AuditRoot   string `yaml:"audit_root"`
	SquidRunDir string `yaml:"squid_run_dir"`
	// CacheRoot holds the persistent per-repo dependency caches for
	// profiles that opt in with cache: true. Per-repo only — caches are
	// never shared across repos.
	CacheRoot string `yaml:"cache_root"`

	// Broker listener
	Broker struct {
		Socket string `yaml:"socket"`
		Addr   string `yaml:"addr"`
	} `yaml:"broker"`

	// Behavior
	Notifications          bool `yaml:"notifications"`
	LogJSON                bool `yaml:"log_json"`
	StrictContainerVersion bool `yaml:"strict_container_version"`
}

// CI watch bounds. The two duration defaults mirror the broker's built-in
// fallbacks (internal/broker/ciwatch.go) so the seeded config documents the
// values the daemon actually applies; a broker-side test pins them equal.
// The minimums and MaxCIAttempts are enforced by validate().
const (
	// DefaultCIPollInterval is the shipped watch tick.
	DefaultCIPollInterval = 60 * time.Second
	// DefaultCIWatchTimeout is the shipped per-PR watch deadline.
	DefaultCIWatchTimeout = 90 * time.Minute
	// MinCIPollInterval floors ci.poll_interval. Each tick spends one GitHub
	// API call per watched PR against the operator's own `gh` credential, so
	// a mistyped "1s" would turn the daemon into a rate-limit-burning loop on
	// a credential drydock does not own.
	MinCIPollInterval = 10 * time.Second
	// MinCIWatchTimeout floors ci.watch_timeout. A window shorter than this
	// cannot contain a single poll, so every watch would expire unobserved
	// and every watched task would dead-letter.
	MinCIWatchTimeout = time.Minute
	// MaxCIAttempts caps ci.max_attempts. The bound exists because a retry
	// chain's worst-case spend is max_attempts × task_budget_usd (each
	// attempt mints a fresh full budget), so a typo'd "10000" would authorize
	// a 10000-deep chain of real model spend. 10 is far past any useful
	// retry depth and still bounds the bill.
	MaxCIAttempts = 10
)

// Defaults returns the same values that the v0.1.0 env-var fallbacks gave.
// Anyone who never edits config.yaml gets exactly that behavior.
func Defaults() *Config {
	c := &Config{
		Network:              "drydock-egress",
		GatewayIP:            "192.168.66.1",
		SandboxImage:         "drydock-sandbox:latest",
		AnchorImage:          "drydock-anchor:latest",
		TaskBudgetUSD:        2.0,
		MaxConcurrent:        2,
		TaskTimeout:          30 * time.Minute,
		DefaultAgent:         "claude",
		AnthropicAuth:        "api_key",
		OpenAIAuth:           "api_key",
		TaskMaxRequests:      0,
		TaskMaxInFlight:      1,
		StageQuotaGB:         8,
		CacheQuotaGB:         20,
		MaxRequestCostUSD:    0,
		AggregateBudgetUSD:   0,
		AggregateWindow:      24 * time.Hour,
		PushMaxRetries:       3,
		PushRetryBackoff:     time.Second,
		PushFreshBranchTries: 2,
		StageRoot:            defaultStateDir("stage"),
		AuditRoot:            defaultStateDir("audit"),
		SquidRunDir:          defaultStateDir("squid"),
		CacheRoot:            defaultStateDir("cache"),
		CI: CIConfig{
			Watch:        false, // explicit: the CI watch ships OFF
			PollInterval: DefaultCIPollInterval,
			WatchTimeout: DefaultCIWatchTimeout,
			MaxAttempts:  0, // explicit: bounded retry (B2) ships OFF
		},
		Notifications:          true,
		LogJSON:                false,
		StrictContainerVersion: false,
	}
	return c
}

// DefaultPath returns ~/.drydock/config.yaml — where drydock init seeds the
// file and where brokerd looks for it at boot.
func DefaultPath() string {
	if d := Dir(); d != "" {
		return filepath.Join(d, "config.yaml")
	}
	return ""
}

// EgressPath returns ~/.drydock/egress.yaml.
func EgressPath() string {
	if d := Dir(); d != "" {
		return filepath.Join(d, "egress.yaml")
	}
	return ""
}

// Dir returns ~/.drydock.
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".drydock")
}

// LockPath returns ~/.drydock/brokerd.lock — the single-instance lock brokerd
// flocks at boot so only one daemon runs per host. Empty if home is
// unresolvable (mirrors DefaultPath).
func LockPath() string {
	if d := Dir(); d != "" {
		return filepath.Join(d, "brokerd.lock")
	}
	return ""
}

// defaultStateDir resolves <~/.drydock>/<sub> for stage/audit/squid runtime
// state. Falls back to /tmp/broker/<sub> only if the home directory is
// unresolvable (rare; happens in some CI/launchd contexts). The point of
// moving off /tmp is so audit history survives — operators digging through
// last week's tasks should find them there, not in a directory tools and
// OS upgrades treat as scratch.
func defaultStateDir(sub string) string {
	if d := Dir(); d != "" {
		return filepath.Join(d, sub)
	}
	return filepath.Join("/tmp", "broker", sub)
}

// expandHome resolves a leading ~ in path fields to the user's home dir.
// YAML doesn't expand shell tildes, but the seeded config and operator
// edits commonly write `~/.drydock/audit`; without this expansion brokerd
// would create a literal directory named "~". Idempotent — paths already
// starting with / are left alone.
func (c *Config) expandHome() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	expand := func(p string) string {
		switch {
		case p == "~":
			return home
		case strings.HasPrefix(p, "~/"):
			return filepath.Join(home, p[2:])
		}
		return p
	}
	c.StageRoot = expand(c.StageRoot)
	c.AuditRoot = expand(c.AuditRoot)
	c.SquidRunDir = expand(c.SquidRunDir)
	c.CacheRoot = expand(c.CacheRoot)
	c.Broker.Socket = expand(c.Broker.Socket)
}

// Load reads `path` (which may not exist) and applies env-var overrides.
// A missing file is not an error — it just yields defaults + env. Parse
// errors and obviously-wrong values DO error so the operator sees them.
func Load(path string) (*Config, error) {
	cfg := Defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			// KnownFields(true): a misspelled key is a hard error, not a silent
			// no-op to a weaker default. In unattended use a typo'd
			// aggregate_budget_usd would otherwise disable the cap with no signal.
			dec := yaml.NewDecoder(bytes.NewReader(b))
			dec.KnownFields(true)
			if err := dec.Decode(cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			// A second YAML document (---) would be silently ignored, and it can
			// carry security config the operator believes is active. Fail closed:
			// exactly one document is allowed (F-08).
			if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("parse %s: trailing YAML document; only one document is allowed", path)
			}
		case os.IsNotExist(err):
			// fine — fall through, env-only / defaults-only
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	cfg.applyEnvOverrides()
	cfg.expandHome()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnvOverrides() {
	// Backwards-compat: every env var documented in README v0.1.x still wins.
	if v := os.Getenv("DRYDOCK_NETWORK"); v != "" {
		c.Network = v
	}
	if v := os.Getenv("DRYDOCK_GW_IP"); v != "" {
		c.GatewayIP = v
	}
	if v := os.Getenv("SANDBOX_IMAGE"); v != "" {
		c.SandboxImage = v
	}
	if v := os.Getenv("DRYDOCK_ANCHOR_IMAGE"); v != "" {
		c.AnchorImage = v
	}
	if v := os.Getenv("DRYDOCK_TASK_BUDGET_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.TaskBudgetUSD = f
		}
	}
	if v := os.Getenv("DRYDOCK_MAX_CONCURRENT_TASKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxConcurrent = n
		}
	}
	if v := os.Getenv("DRYDOCK_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("DRYDOCK_DEFAULT_AGENT"); v != "" {
		c.DefaultAgent = v
	}
	if v := os.Getenv("DRYDOCK_ANTHROPIC_AUTH"); v != "" {
		c.AnthropicAuth = v
	}
	if v := os.Getenv("DRYDOCK_OPENAI_AUTH"); v != "" {
		c.OpenAIAuth = v
	}
	if v := os.Getenv("DRYDOCK_TASK_MAX_REQUESTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.TaskMaxRequests = n
		}
	}
	if v := os.Getenv("DRYDOCK_TASK_MAX_INFLIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.TaskMaxInFlight = n
		}
	}
	if v := os.Getenv("DRYDOCK_STAGE_QUOTA_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.StageQuotaGB = n
		}
	}
	if v := os.Getenv("DRYDOCK_CACHE_QUOTA_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.CacheQuotaGB = n
		}
	}
	if v := os.Getenv("DRYDOCK_AGGREGATE_BUDGET_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.AggregateBudgetUSD = f
		}
	}
	if v := os.Getenv("DRYDOCK_MAX_REQUEST_COST_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.MaxRequestCostUSD = f
		}
	}
	if v := os.Getenv("DRYDOCK_AGGREGATE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.AggregateWindow = d
		}
	}
	if v := os.Getenv("DRYDOCK_PUSH_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.PushMaxRetries = n
		}
	}
	if v := os.Getenv("DRYDOCK_PUSH_RETRY_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.PushRetryBackoff = d
		}
	}
	if v := os.Getenv("DRYDOCK_PUSH_FRESH_BRANCH_TRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.PushFreshBranchTries = n
		}
	}
	// DRYDOCK_CI_WATCH follows the exactly-"1" bool idiom used by the other
	// three bool vars: nothing else arms the credentialed poll loop.
	if os.Getenv("DRYDOCK_CI_WATCH") == "1" {
		c.CI.Watch = true
	}
	if v := os.Getenv("DRYDOCK_CI_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.CI.PollInterval = d
		}
	}
	if v := os.Getenv("DRYDOCK_CI_WATCH_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.CI.WatchTimeout = d
		}
	}
	if v := os.Getenv("DRYDOCK_CI_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.CI.MaxAttempts = n
		}
	}
	if v := os.Getenv("STAGE_ROOT"); v != "" {
		c.StageRoot = v
	}
	if v := os.Getenv("AUDIT_ROOT"); v != "" {
		c.AuditRoot = v
	}
	if v := os.Getenv("SQUID_RUN_DIR"); v != "" {
		c.SquidRunDir = v
	}
	if v := os.Getenv("DRYDOCK_CACHE_ROOT"); v != "" {
		c.CacheRoot = v
	}
	if v := os.Getenv("BROKER_SOCKET"); v != "" {
		c.Broker.Socket = v
	}
	if v := os.Getenv("BROKER_ADDR"); v != "" {
		c.Broker.Addr = v
	}
	if os.Getenv("DRYDOCK_NO_NOTIFY") == "1" {
		c.Notifications = false
	}
	if os.Getenv("DRYDOCK_LOG_JSON") == "1" {
		c.LogJSON = true
	}
	if os.Getenv("DRYDOCK_STRICT_CONTAINER_VERSION") == "1" {
		c.StrictContainerVersion = true
	}
}

func (c *Config) validate() error {
	if c.Network == "" {
		return errors.New("config: network is required")
	}
	if c.GatewayIP == "" {
		return errors.New("config: gateway_ip is required")
	}
	if c.MaxConcurrent < 1 {
		return errors.New("config: max_concurrent_tasks must be ≥ 1")
	}
	if c.TaskBudgetUSD <= 0 {
		return errors.New("config: task_budget_usd must be positive")
	}
	if c.TaskTimeout < time.Second {
		return errors.New("config: task_timeout must be ≥ 1s")
	}
	if c.ApprovalTimeout != 0 && c.ApprovalTimeout < time.Second {
		return errors.New("config: approval_timeout must be 0 (wait indefinitely) or ≥ 1s")
	}
	if _, ok := provider.ByAgent(c.DefaultAgent); !ok {
		return fmt.Errorf("config: default_agent must be one of %v, got %q", provider.Agents(), c.DefaultAgent)
	}
	if c.AnthropicAuth != "api_key" && c.AnthropicAuth != "subscription" {
		return fmt.Errorf("config: anthropic_auth must be api_key or subscription, got %q", c.AnthropicAuth)
	}
	if c.OpenAIAuth != "api_key" && c.OpenAIAuth != "subscription" {
		return fmt.Errorf("config: openai_auth must be api_key or subscription, got %q", c.OpenAIAuth)
	}
	if c.TaskMaxRequests < 0 {
		return fmt.Errorf("config: task_max_requests must be >= 0, got %d", c.TaskMaxRequests)
	}
	if c.TaskMaxInFlight < 0 {
		return fmt.Errorf("config: task_max_inflight must be >= 0, got %d", c.TaskMaxInFlight)
	}
	if c.StageQuotaGB < 0 {
		return fmt.Errorf("config: stage_quota_gb must be >= 0, got %d", c.StageQuotaGB)
	}
	if c.CacheQuotaGB < 0 {
		return fmt.Errorf("config: cache_quota_gb must be >= 0, got %d", c.CacheQuotaGB)
	}
	if oc := c.OpenAICompat; oc.BaseURL != "" {
		if oc.APIKeyEnv == "" || oc.Model == "" {
			return fmt.Errorf("config: openai_compat.base_url set but api_key_env and model are required")
		}
		u, err := url.Parse(oc.BaseURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("config: openai_compat.base_url must be an absolute URL, got %q", oc.BaseURL)
		}
		isLocal := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
		if u.Scheme != "https" && !(u.Scheme == "http" && isLocal) {
			return fmt.Errorf("config: openai_compat.base_url must be https (http allowed only for localhost), got %q", oc.BaseURL)
		}
		// A negative price is never legitimate: it makes SpentUSD decrease so the
		// USD budget never trips — an outright budget defeat. Reject it. (A
		// missing "default" row stays a warning — the request cap bounds that
		// case — but a negative number is a config error.)
		for model, pr := range oc.Prices {
			if pr.Input < 0 || pr.Output < 0 {
				return fmt.Errorf("config: openai_compat.prices[%q] has a negative value; a negative price disables the USD budget", model)
			}
		}
	}
	for key, vr := range c.Verify.Repos {
		if canon := repokey.Normalize(key); canon != key {
			return fmt.Errorf("config: verify.repos key %q is not canonical; use %q", key, canon)
		}
		if len(vr.Commands) == 0 {
			return fmt.Errorf("config: verify.repos[%q] needs at least one command", key)
		}
		for i, argv := range vr.Commands {
			if len(argv) == 0 || argv[0] == "" {
				return fmt.Errorf("config: verify.repos[%q].commands[%d] is empty", key, i)
			}
		}
		if vr.Timeout < 0 {
			return fmt.Errorf("config: verify.repos[%q].timeout must be >= 0", key)
		}
	}
	for key, sp := range c.Profiles.Repos {
		if canon := repokey.Normalize(key); canon != key {
			return fmt.Errorf("config: profiles.repos key %q is not canonical; use %q", key, canon)
		}
		// A profile with no setup commands is pointless — reject loudly.
		if len(sp.Setup) == 0 {
			return fmt.Errorf("config: profiles.repos[%q] needs at least one setup command", key)
		}
		for i, argv := range sp.Setup {
			if len(argv) == 0 || argv[0] == "" {
				return fmt.Errorf("config: profiles.repos[%q].setup[%d] is empty", key, i)
			}
		}
		for i, argv := range sp.Readiness {
			if len(argv) == 0 || argv[0] == "" {
				return fmt.Errorf("config: profiles.repos[%q].readiness[%d] is empty", key, i)
			}
		}
		if sp.Timeout < 0 {
			return fmt.Errorf("config: profiles.repos[%q].timeout must be >= 0", key)
		}
	}
	if c.DiffPolicy.MaxFilesChanged < 0 {
		return fmt.Errorf("config: diff_policy.max_files_changed must be >= 0, got %d", c.DiffPolicy.MaxFilesChanged)
	}
	if c.DiffPolicy.MaxLinesChanged < 0 {
		return fmt.Errorf("config: diff_policy.max_lines_changed must be >= 0, got %d", c.DiffPolicy.MaxLinesChanged)
	}
	for _, g := range []struct {
		field string
		pats  []string
	}{
		{"blocked_paths", c.DiffPolicy.BlockedPaths},
		{"second_look_paths", c.DiffPolicy.SecondLookPaths},
	} {
		field := g.field
		for i, p := range g.pats {
			if err := globmatch.Valid(p); err != nil {
				return fmt.Errorf("config: diff_policy.%s[%d] invalid glob: %v", field, i, err)
			}
			// globmatch treats a trailing "/" as an empty final segment, so
			// "dir/" is well-formed but matches nothing — silently inert
			// policy. Make it a loud config error instead.
			if strings.HasSuffix(p, "/") {
				return fmt.Errorf("config: diff_policy.%s[%d] %q ends with %q and matches nothing; use %q to match everything under the directory", field, i, p, "/", p+"**")
			}
		}
	}
	if c.AggregateBudgetUSD < 0 {
		return fmt.Errorf("config: aggregate_budget_usd must be >= 0, got %v", c.AggregateBudgetUSD)
	}
	if c.MaxRequestCostUSD < 0 {
		return fmt.Errorf("config: max_request_cost_usd must be >= 0, got %v", c.MaxRequestCostUSD)
	}
	if c.AggregateWindow < 0 {
		return fmt.Errorf("config: aggregate_window must be >= 0, got %v", c.AggregateWindow)
	}
	if c.PushMaxRetries < 0 {
		return fmt.Errorf("config: push_max_retries must be >= 0, got %d", c.PushMaxRetries)
	}
	if c.PushRetryBackoff < 0 {
		return fmt.Errorf("config: push_retry_backoff must be >= 0, got %v", c.PushRetryBackoff)
	}
	if c.PushFreshBranchTries < 0 {
		return fmt.Errorf("config: push_fresh_branch_tries must be >= 0, got %d", c.PushFreshBranchTries)
	}
	// ci: the range checks are deliberately explicit rather than "clamp to
	// something sane". A silently-clamped poll interval or attempt cap is a
	// security control the operator believes they set and did not.
	if c.CI.PollInterval != 0 && c.CI.PollInterval < MinCIPollInterval {
		return fmt.Errorf("config: ci.poll_interval must be 0 (built-in default %v) or >= %v, got %v",
			DefaultCIPollInterval, MinCIPollInterval, c.CI.PollInterval)
	}
	if c.CI.WatchTimeout != 0 && c.CI.WatchTimeout < MinCIWatchTimeout {
		return fmt.Errorf("config: ci.watch_timeout must be 0 (built-in default %v) or >= %v, got %v",
			DefaultCIWatchTimeout, MinCIWatchTimeout, c.CI.WatchTimeout)
	}
	if c.CI.MaxAttempts < 0 {
		return fmt.Errorf("config: ci.max_attempts must be >= 0, got %d", c.CI.MaxAttempts)
	}
	if c.CI.MaxAttempts > MaxCIAttempts {
		return fmt.Errorf("config: ci.max_attempts must be <= %d, got %d; each attempt mints a fresh task_budget_usd, so the worst-case spend for one chain is max_attempts × task_budget_usd",
			MaxCIAttempts, c.CI.MaxAttempts)
	}
	return nil
}

// SeedTemplate is the comment-rich YAML written by `drydock init` when
// ~/.drydock/config.yaml is missing. Values match Defaults() so the file
// fully documents what the daemon does on boot.
const SeedTemplate = `# drydock configuration. Re-run ` + "`" + `drydock start` + "`" + ` after editing.
#
# Env vars override these values (e.g. setting BROKER_ADDR in the shell
# wins over broker.addr below). ANTHROPIC_API_KEY is intentionally not in
# this file — it never goes to disk.

# --- Container runtime ---
network:        drydock-egress         # vmnet network name (must exist)
gateway_ip:     192.168.66.1           # gateway + squid bind here
sandbox_image:  drydock-sandbox:latest # per-task agent VM image
anchor_image:   drydock-anchor:latest  # minimal anchor holding the vmnet gateway IP

# --- Per-task limits ---
task_budget_usd:        2.0            # soft USD cap: metered post-hoc; overshoot bounded to task_max_inflight in-flight requests (default 1); set max_request_cost_usd for a reservation-backed bound (api_key mode only; ignored in subscription mode)
max_concurrent_tasks:   2              # excess POSTs /tasks get HTTP 503
task_timeout:           30m            # wall-clock per task
approval_timeout:       0s             # auto-deny a task waiting at an approval gate after this long (0 = wait forever; set for unattended runs)
default_model:          ""             # model fallback for Claude Code and Codex (e.g. claude-sonnet-4-6); empty = agent picks. opencode uses openai_compat.model instead. Per-task --model overrides.
default_agent:          claude         # sandbox CLI: claude | codex | gemini | opencode. Per-task --agent overrides.
anthropic_auth:         api_key        # authentication mode: api_key | subscription
openai_auth:            api_key        # authentication mode: api_key | subscription
task_max_requests:      0              # per-task request cap. 0 falls closed to a built-in default (1000) in every mode; set explicitly to change the bound
task_max_inflight:      1              # concurrent gateway requests per task lease; bounds budget overshoot to this many in-flight requests (0 = unlimited)
stage_quota_gb:         8              # hard per-task disk bound (GiB): /work lives in an APFS sparse image this big (macOS). 0 = plain host dir, polling guard only
cache_quota_gb:         20             # total disk bound (GiB) for the persistent dependency cache under cache_root. 0 = cache disabled entirely, even for repos with cache: true
max_request_cost_usd:   0              # worst-case USD reserved per in-flight request so concurrent requests can't admit past the budget; 0 = disabled (post-hoc metering only)
aggregate_budget_usd:   0              # cross-task USD ceiling per api_key provider over aggregate_window; 0 = disabled. subscription is out of scope (bounded per task by task_max_requests)
aggregate_window:       24h            # rolling window for aggregate_budget_usd; 0 = total since brokerd boot (resets on restart)
push_max_retries:       3              # retry a transient (network) push failure N times with backoff; 0 = no retry
push_retry_backoff:     1s             # base delay for push retry backoff (doubles each retry)
push_fresh_branch_tries: 2             # on a branch-name collision, try agent/<id>-2, -3, ...; 0 = no fresh-branch retry

# --- Bring-your-own OpenAI-compatible model (optional; e.g. Gemini, OpenRouter, local) ---
openai_compat:
  base_url:    ""        # e.g. https://generativelanguage.googleapis.com  (empty = disabled)
  base_path:   ""        # e.g. /v1beta/openai
  api_key_env: ""        # name of the host env var holding the real key (never the key itself)
  model:       ""        # model id passed to the agent, e.g. gemini-2.5-pro
  # prices:              # USD/1M-token rates for budget metering (optional)
  #   gemini-2.5-pro: {input: 1.25, output: 10.00}

# verify: independent post-run verification. Commands run against the agent's
# exact staged tree in a fresh VM with no credentials and no network; exit
# codes are recorded in the task's trust brief. required: true blocks the
# push unless every command passes. Keys are canonical host/owner/repo.
# verify:
#   repos:
#     "github.com/you/yourrepo":
#       commands:
#         - ["go", "test", "./..."]
#       timeout: 10m      # 0 = default (10m)
#       required: false   # true = failure/inconclusive blocks push

# profiles: host-side execution profiles. Setup commands run inside the task
# VM against the staged tree BEFORE the agent starts; readiness commands then
# gate the run. Any failure fails the task closed before any model spend.
# Commands live in this host config only — the sandboxed agent cannot edit
# them. Each command is a self-contained argv: shell state (cd, exports) does
# not carry between commands. Set cache: true to opt a repo into the
# persistent dependency cache under cache_root: setup can reuse dependencies
# downloaded by earlier tasks instead of starting from scratch. The cache is
# strictly per-repo (never shared across repos), is read-only to the agent,
# and requires the repo to have a lockfile so reuse is pinned to exact
# dependency versions. Default false — setup runs from scratch every task.
# Keys are canonical host/owner/repo.
# profiles:
#   repos:
#     "github.com/you/yourrepo":
#       setup:
#         - ["npm", "ci"]
#       readiness:
#         - ["node", "--version"]
#       timeout: 10m      # 0 = default
#       cache: false      # true = opt in to the persistent per-repo dependency cache

# diff_policy: host-side caps on the diff a task may propose. A diff that
# exceeds a cap or touches a blocked path fails the task closed as
# policy_blocked BEFORE it reaches review; second-look paths still reach
# review, but the approver must acknowledge each flagged category. Path
# patterns are **-aware repo-relative globs: write "dir/**" to cover
# everything under dir — a trailing-slash pattern like "dir/" matches
# nothing and is rejected at load. Omit the block (or leave zeros/empty
# lists) to disable every check.
# diff_policy:
#   max_files_changed: 0        # fail closed when the diff changes more files than this (0 = no cap)
#   max_lines_changed: 0        # fail closed when added+deleted lines exceed this (0 = no cap)
#   blocked_paths: []           # e.g. ["**/*.pem", ".github/workflows/**"] — touching one fails the task
#   second_look_paths: []       # e.g. ["**/Dockerfile"] — approver must acknowledge each flagged category

# --- Host-side CI observation (opt-in; OFF by default) ---
# With watch: true, a queued task no longer finishes at the push: once its
# branch is pushed and the PR is open the queue item moves to awaiting_ci and
# brokerd polls that PR's check CONCLUSIONS until they resolve. Only
# conclusions are read — no CI log text is fetched, stored, or parsed, and
# nothing a repository's workflow prints can influence a decision. The watch
# holds no concurrency slot and keeps no stage. An observed pass (or an
# observed "this PR has no checks") completes the item; an observed failure is
# the new terminal ci_failed; a watch that ends with NO conclusion (timeout,
# repeated read failures, marker lost to a restart) dead-letters — absence of
# evidence is never success.
#
# SECURITY: turning this on puts the HOST's gh credential on a TIMER. It is
# read-only ("gh pr checks" / "gh pr view --json"), pinned to github.com, and
# uses the same curated env as every other host CLI call — but it is no longer
# only operator-initiated. See THREAT_MODEL.md N5. Set watch: false (the
# default) to stop it entirely.
ci:
  watch:         false          # enable the host-side CI watch (default false = off; nothing polls, no gh call on a timer)
  poll_interval: 60s            # watch tick; one gh API call per watched PR per tick. 0 = built-in default (60s); minimum 10s
  watch_timeout: 90m            # ABSOLUTE per-PR deadline anchored at push, so a restart can't extend it. 0 = built-in default (90m); minimum 1m
  max_attempts:  0              # bounded retry on an observed CI failure (increment B2). 0 = off. Worst-case spend for one chain is max_attempts × task_budget_usd; max 10

# --- Where state lives ---
stage_root:    ~/.drydock/stage        # per-task work tree (wiped on completion)
audit_root:    ~/.drydock/audit        # per-task <id>.jsonl + .diff
squid_run_dir: ~/.drydock/squid        # squid pid/conf/cache.log
cache_root:    ~/.drydock/cache        # persistent per-repo dependency caches for profiles with cache: true (bounded by cache_quota_gb)

# --- Broker listener ---
broker:
  socket: ""                           # empty = per-uid default ($TMPDIR/drydock-$UID/drydock.sock)
  addr: ""                             # set "127.0.0.1:PORT" for loopback TCP (non-loopback is refused; SSH-forward for remote)

# --- Behavior ---
notifications:              true       # macOS notifications on pending approval
log_json:                   false      # force JSON logs (default: text on TTY, JSON otherwise)
strict_container_version:   false      # fail closed when 'container' major drifts from tested range
`

// WriteSeed writes SeedTemplate to path, creating parents at 0700 and the
// file at 0644. Used by drydock init. Refuses to overwrite — caller checks.
func WriteSeed(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(SeedTemplate), 0o644)
}

// AuthMode returns the configured auth mode ("api_key" | "subscription") for a
// gateway vendor, reading the typed per-vendor field. Unknown vendor -> "".
func (c *Config) AuthMode(vendor string) string {
	switch vendor {
	case "anthropic":
		return c.AnthropicAuth
	case "openai":
		return c.OpenAIAuth
	default:
		return ""
	}
}
