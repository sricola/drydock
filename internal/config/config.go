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
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"drydock/internal/provider"
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

	// TaskMaxRequests is a per-task request cap (0 = unlimited).
	TaskMaxRequests int `yaml:"task_max_requests"`

	// OpenAICompat configures a bring-your-own OpenAI-compatible upstream
	// (Gemini's /v1beta/openai, OpenRouter, local). Empty BaseURL = disabled.
	// The real key is read from the host env var named by APIKeyEnv — never
	// stored here. Prices (USD per 1M tokens) enable USD metering; omit to fall
	// back to the task_max_requests cap.
	OpenAICompat OpenAICompatConfig `yaml:"openai_compat"`

	// Where state lives
	StageRoot   string `yaml:"stage_root"`
	AuditRoot   string `yaml:"audit_root"`
	SquidRunDir string `yaml:"squid_run_dir"`

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

// Defaults returns the same values that the v0.1.0 env-var fallbacks gave.
// Anyone who never edits config.yaml gets exactly that behavior.
func Defaults() *Config {
	c := &Config{
		Network:                "drydock-egress",
		GatewayIP:              "192.168.66.1",
		SandboxImage:           "drydock-sandbox:latest",
		AnchorImage:            "drydock-anchor:latest",
		TaskBudgetUSD:          2.0,
		MaxConcurrent:          2,
		TaskTimeout:            30 * time.Minute,
		DefaultAgent:           "claude",
		AnthropicAuth:          "api_key",
		OpenAIAuth:             "api_key",
		TaskMaxRequests:        0,
		StageRoot:              defaultStateDir("stage"),
		AuditRoot:              defaultStateDir("audit"),
		SquidRunDir:            defaultStateDir("squid"),
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
			if err := yaml.Unmarshal(b, cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
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
		if n, err := strconv.Atoi(v); err == nil {
			c.TaskMaxRequests = n
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
task_budget_usd:        2.0            # hard USD ceiling; gateway rejects after exhaustion (api_key mode only; ignored in subscription mode)
max_concurrent_tasks:   2              # excess POSTs /tasks get HTTP 503
task_timeout:           30m            # wall-clock per task
approval_timeout:       0s             # auto-deny a task waiting at an approval gate after this long (0 = wait forever; set for unattended runs)
default_model:          ""             # model fallback for Claude Code and Codex (e.g. claude-sonnet-4-6); empty = agent picks. opencode uses openai_compat.model instead. Per-task --model overrides.
default_agent:          claude         # sandbox CLI: claude | codex | gemini | opencode. Per-task --agent overrides.
anthropic_auth:         api_key        # authentication mode: api_key | subscription
openai_auth:            api_key        # authentication mode: api_key | subscription
task_max_requests:      0              # per-task request cap (0 = unlimited)

# --- Bring-your-own OpenAI-compatible model (optional; e.g. Gemini, OpenRouter, local) ---
openai_compat:
  base_url:    ""        # e.g. https://generativelanguage.googleapis.com  (empty = disabled)
  base_path:   ""        # e.g. /v1beta/openai
  api_key_env: ""        # name of the host env var holding the real key (never the key itself)
  model:       ""        # model id passed to the agent, e.g. gemini-2.5-pro
  # prices:              # USD/1M-token rates for budget metering (optional)
  #   gemini-2.5-pro: {input: 1.25, output: 10.00}

# --- Where state lives ---
stage_root:    ~/.drydock/stage        # per-task work tree (wiped on completion)
audit_root:    ~/.drydock/audit        # per-task <id>.jsonl + .diff
squid_run_dir: ~/.drydock/squid        # squid pid/conf/cache.log

# --- Broker listener ---
broker:
  socket: ""                           # empty = per-uid default ($TMPDIR/drydock-$UID/drydock.sock)
  addr: ""                             # set "host:port" to expose over TCP (warns at boot)

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
