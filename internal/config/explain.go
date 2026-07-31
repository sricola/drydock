package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Source identifies which layer of the resolution order (env > yaml > default)
// supplied a field's effective value.
type Source string

const (
	SourceDefault Source = "default"
	SourceYAML    Source = "config.yaml"
	SourceEnv     Source = "env"
)

// Field is one config field's provenance: its effective (resolved) value and
// which layer supplied it. EnvVar is empty for yaml-only fields.
type Field struct {
	Name    string `json:"name"`
	YAMLKey string `json:"yaml_key"`
	EnvVar  string `json:"env_var"`
	Value   string `json:"value"`
	Source  Source `json:"source"`
}

// fieldDesc describes one config field for the provenance table. guardedEnv
// re-runs the EXACT guard applyEnvOverrides uses for that env var, so a
// set-but-invalid env var correctly falls through to yaml/default. differs
// reports whether the yaml-decoded config sets the field to a non-default;
// nil means "compare the rendered values".
type fieldDesc struct {
	name       string
	yamlKey    string
	envVar     string
	guardedEnv func() (string, bool)
	value      func(*Config) string
	differs    func(yamlCfg, def *Config) bool
}

// Guard helpers — each mirrors one guard shape in applyEnvOverrides. Any
// drift between these and applyEnvOverrides is a silent wrong attribution,
// which is why TestExplain_CoversEveryEnvOverride pins the env-var set.

// envString: applies whenever the var is non-empty.
func envString(key string) func() (string, bool) {
	return func() (string, bool) {
		v := os.Getenv(key)
		return v, v != ""
	}
}

// envFloatPositive: ParseFloat succeeds AND f > 0 (DRYDOCK_TASK_BUDGET_USD).
func envFloatPositive(key string) func() (string, bool) {
	return func() (string, bool) {
		v := os.Getenv(key)
		if v == "" {
			return "", false
		}
		f, err := strconv.ParseFloat(v, 64)
		return v, err == nil && f > 0
	}
}

// envFloatNonNegative: ParseFloat succeeds AND f >= 0.
func envFloatNonNegative(key string) func() (string, bool) {
	return func() (string, bool) {
		v := os.Getenv(key)
		if v == "" {
			return "", false
		}
		f, err := strconv.ParseFloat(v, 64)
		return v, err == nil && f >= 0
	}
}

// envIntPositive: Atoi succeeds AND n > 0 (DRYDOCK_MAX_CONCURRENT_TASKS).
func envIntPositive(key string) func() (string, bool) {
	return func() (string, bool) {
		v := os.Getenv(key)
		if v == "" {
			return "", false
		}
		n, err := strconv.Atoi(v)
		return v, err == nil && n > 0
	}
}

// envIntNonNegative: Atoi succeeds AND n >= 0.
func envIntNonNegative(key string) func() (string, bool) {
	return func() (string, bool) {
		v := os.Getenv(key)
		if v == "" {
			return "", false
		}
		n, err := strconv.Atoi(v)
		return v, err == nil && n >= 0
	}
}

// envDurationNonNegative: ParseDuration succeeds AND d >= 0.
func envDurationNonNegative(key string) func() (string, bool) {
	return func() (string, bool) {
		v := os.Getenv(key)
		if v == "" {
			return "", false
		}
		d, err := time.ParseDuration(v)
		return v, err == nil && d >= 0
	}
}

// envBoolOne: applies only when the var is exactly "1" (the three bool vars).
func envBoolOne(key string) func() (string, bool) {
	return func() (string, bool) {
		v := os.Getenv(key)
		return v, v == "1"
	}
}

// Value renderers — every value is a single line.

func renderFloat(f float64) string     { return strconv.FormatFloat(f, 'g', -1, 64) }
func renderInt(n int) string           { return strconv.Itoa(n) }
func renderBool(b bool) string         { return strconv.FormatBool(b) }
func renderDur(d time.Duration) string { return d.String() }

// renderOpenAICompat is a compact one-line summary of the openai_compat block.
func renderOpenAICompat(oc OpenAICompatConfig) string {
	if oc.BaseURL == "" {
		return "disabled"
	}
	return "base_url set"
}

// renderVerifyRepos is a one-line count+keys summary of verify.repos.
func renderVerifyRepos(repos map[string]VerifyRepo) string {
	if len(repos) == 0 {
		return "0 repos"
	}
	keys := make([]string, 0, len(repos))
	for k := range repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("%d repos: %s", len(keys), strings.Join(keys, ", "))
}

// renderProfiles is a compact one-line summary of profiles.repos: counts plus
// a content hash. Like renderDiffPolicy's glob lists, the counts alone would
// be content-blind: EffectiveHash hashes this rendered value, so two profile
// configs with identical counts but different COMMAND content (the daemon
// runs `npm ci`, the edited config says something else) would make `drydock
// policy explain` falsely report "in sync". Setup commands run pre-agent with
// egress — divergence there must trip the DIVERGENT verdict.
func renderProfiles(repos map[string]SetupProfile) string {
	if len(repos) == 0 {
		return "disabled"
	}
	setup, readiness := 0, 0
	for _, sp := range repos {
		setup += len(sp.Setup)
		readiness += len(sp.Readiness)
	}
	return fmt.Sprintf("%d repos / %d setup / %d readiness (%s)",
		len(repos), setup, readiness, profilesHash(repos))
}

// profilesHash is the first 8 hex chars of sha256 over a canonical rendering
// of the profiles map: one block per repo — the repo key, the repo's timeout
// ("timeout=<dur>": it is a documented hard bound on setup execution, so a
// daemon-vs-config timeout edit must read as DIVERGENT), the repo's cache
// opt-in ("cache=<bool>": it decides whether setup state persists across
// tasks, so a daemon-vs-config cache toggle must read as DIVERGENT too), a
// "setup" marker,
// each setup command, a "readiness" marker, each readiness command —
// newline-joined, with the per-repo blocks ordered by SORTED repo key.
// Sorting the blocks makes repo ordering irrelevant (maps are unordered; two
// loads of the same yaml must hash identically), but command order WITHIN a
// repo's setup/readiness list is deliberately preserved: execution order is
// significant (`npm ci` before `npm run build`), so reordering commands is a
// real config change and must change the hash. The section markers make a
// command moving between setup and readiness change the hash too. Each argv
// element is strconv.Quote'd before joining so element boundaries are
// unambiguous — a plain space-join would make ["sh","-c","a b"] and
// ["sh","-c","a","b"] collide, hiding an argv-boundary edit. Operates on
// sorted key copies only — the caller's map and slices are never mutated.
func profilesHash(repos map[string]SetupProfile) string {
	keys := make([]string, 0, len(repos))
	for k := range repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		sp := repos[k]
		b.WriteString(k)
		b.WriteString("\ntimeout=")
		b.WriteString(renderDur(sp.Timeout))
		b.WriteString("\ncache=")
		b.WriteString(renderBool(sp.Cache))
		b.WriteString("\nsetup\n")
		for _, cmd := range sp.Setup {
			b.WriteString(quoteArgv(cmd))
			b.WriteByte('\n')
		}
		b.WriteString("readiness\n")
		for _, cmd := range sp.Readiness {
			b.WriteString(quoteArgv(cmd))
			b.WriteByte('\n')
		}
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])[:8]
}

// quoteArgv renders an argv with each element strconv.Quote'd, making the
// encoding injective over element boundaries (see profilesHash).
func quoteArgv(cmd []string) string {
	quoted := make([]string, len(cmd))
	for i, a := range cmd {
		quoted[i] = strconv.Quote(a)
	}
	return strings.Join(quoted, " ")
}

// renderDiffPolicy is a compact one-line summary of the diff_policy block.
// The glob lists render as count + content hash rather than count alone:
// EffectiveHash and the policy-explain divergence diff both consume this
// rendered value, so two configs whose blocked/second-look globs differ in
// CONTENT but not count must not render identically — a bare count would make
// `drydock policy explain` falsely report "in sync" while the daemon enforces
// different blocked-path globs.
func renderDiffPolicy(dp DiffPolicy) string {
	if !diffPolicySet(dp) {
		return "disabled"
	}
	return fmt.Sprintf("%d files / %d lines / %s / %s",
		dp.MaxFilesChanged, dp.MaxLinesChanged,
		globsSummary(dp.BlockedPaths, "blocked"),
		globsSummary(dp.SecondLookPaths, "second-look"))
}

// globsSummary renders one glob list as `N label (hash)`, omitting the parens
// entirely when the list is empty.
func globsSummary(globs []string, label string) string {
	if len(globs) == 0 {
		return "0 " + label
	}
	return fmt.Sprintf("%d %s (%s)", len(globs), label, globsHash(globs))
}

// globsHash is the first 8 hex chars of sha256 over the SORTED glob list
// joined with "\n", or "" for an empty list. Sorting makes the hash
// order-insensitive — reordering semantically identical globs in config must
// not report divergence — and operates on a copy so the caller's slice is
// never mutated.
func globsHash(globs []string) string {
	if len(globs) == 0 {
		return ""
	}
	sorted := append([]string(nil), globs...)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(h[:])[:8]
}

// diffPolicySet reports whether any diff_policy field is non-zero (the zero
// value disables the whole block).
func diffPolicySet(dp DiffPolicy) bool {
	return dp.MaxFilesChanged != 0 || dp.MaxLinesChanged != 0 ||
		len(dp.BlockedPaths) != 0 || len(dp.SecondLookPaths) != 0
}

// provenanceTable enumerates every field of Config in declaration order,
// pairing each with its yaml key, env var (or ""), guard, and renderer.
// This table must stay in lockstep with applyEnvOverrides and Defaults();
// TestExplain_CoversEveryEnvOverride guards the env-var half of that.
func provenanceTable() []fieldDesc {
	return []fieldDesc{
		{name: "Network", yamlKey: "network", envVar: "DRYDOCK_NETWORK",
			guardedEnv: envString("DRYDOCK_NETWORK"),
			value:      func(c *Config) string { return c.Network }},
		{name: "GatewayIP", yamlKey: "gateway_ip", envVar: "DRYDOCK_GW_IP",
			guardedEnv: envString("DRYDOCK_GW_IP"),
			value:      func(c *Config) string { return c.GatewayIP }},
		{name: "SandboxImage", yamlKey: "sandbox_image", envVar: "SANDBOX_IMAGE",
			guardedEnv: envString("SANDBOX_IMAGE"),
			value:      func(c *Config) string { return c.SandboxImage }},
		{name: "AnchorImage", yamlKey: "anchor_image", envVar: "DRYDOCK_ANCHOR_IMAGE",
			guardedEnv: envString("DRYDOCK_ANCHOR_IMAGE"),
			value:      func(c *Config) string { return c.AnchorImage }},
		{name: "TaskBudgetUSD", yamlKey: "task_budget_usd", envVar: "DRYDOCK_TASK_BUDGET_USD",
			guardedEnv: envFloatPositive("DRYDOCK_TASK_BUDGET_USD"),
			value:      func(c *Config) string { return renderFloat(c.TaskBudgetUSD) }},
		{name: "MaxConcurrent", yamlKey: "max_concurrent_tasks", envVar: "DRYDOCK_MAX_CONCURRENT_TASKS",
			guardedEnv: envIntPositive("DRYDOCK_MAX_CONCURRENT_TASKS"),
			value:      func(c *Config) string { return renderInt(c.MaxConcurrent) }},
		{name: "TaskTimeout", yamlKey: "task_timeout",
			value: func(c *Config) string { return renderDur(c.TaskTimeout) }},
		{name: "ApprovalTimeout", yamlKey: "approval_timeout",
			value: func(c *Config) string { return renderDur(c.ApprovalTimeout) }},
		{name: "DefaultModel", yamlKey: "default_model", envVar: "DRYDOCK_DEFAULT_MODEL",
			guardedEnv: envString("DRYDOCK_DEFAULT_MODEL"),
			value:      func(c *Config) string { return c.DefaultModel }},
		{name: "DefaultAgent", yamlKey: "default_agent", envVar: "DRYDOCK_DEFAULT_AGENT",
			guardedEnv: envString("DRYDOCK_DEFAULT_AGENT"),
			value:      func(c *Config) string { return c.DefaultAgent }},
		{name: "AnthropicAuth", yamlKey: "anthropic_auth", envVar: "DRYDOCK_ANTHROPIC_AUTH",
			guardedEnv: envString("DRYDOCK_ANTHROPIC_AUTH"),
			value:      func(c *Config) string { return c.AnthropicAuth }},
		{name: "OpenAIAuth", yamlKey: "openai_auth", envVar: "DRYDOCK_OPENAI_AUTH",
			guardedEnv: envString("DRYDOCK_OPENAI_AUTH"),
			value:      func(c *Config) string { return c.OpenAIAuth }},
		{name: "TaskMaxRequests", yamlKey: "task_max_requests", envVar: "DRYDOCK_TASK_MAX_REQUESTS",
			guardedEnv: envIntNonNegative("DRYDOCK_TASK_MAX_REQUESTS"),
			value:      func(c *Config) string { return renderInt(c.TaskMaxRequests) }},
		{name: "TaskMaxInFlight", yamlKey: "task_max_inflight", envVar: "DRYDOCK_TASK_MAX_INFLIGHT",
			guardedEnv: envIntNonNegative("DRYDOCK_TASK_MAX_INFLIGHT"),
			value:      func(c *Config) string { return renderInt(c.TaskMaxInFlight) }},
		{name: "StageQuotaGB", yamlKey: "stage_quota_gb", envVar: "DRYDOCK_STAGE_QUOTA_GB",
			guardedEnv: envIntNonNegative("DRYDOCK_STAGE_QUOTA_GB"),
			value:      func(c *Config) string { return renderInt(c.StageQuotaGB) }},
		{name: "CacheQuotaGB", yamlKey: "cache_quota_gb", envVar: "DRYDOCK_CACHE_QUOTA_GB",
			guardedEnv: envIntNonNegative("DRYDOCK_CACHE_QUOTA_GB"),
			value:      func(c *Config) string { return renderInt(c.CacheQuotaGB) }},
		{name: "MaxRequestCostUSD", yamlKey: "max_request_cost_usd", envVar: "DRYDOCK_MAX_REQUEST_COST_USD",
			guardedEnv: envFloatNonNegative("DRYDOCK_MAX_REQUEST_COST_USD"),
			value:      func(c *Config) string { return renderFloat(c.MaxRequestCostUSD) }},
		{name: "AggregateBudgetUSD", yamlKey: "aggregate_budget_usd", envVar: "DRYDOCK_AGGREGATE_BUDGET_USD",
			guardedEnv: envFloatNonNegative("DRYDOCK_AGGREGATE_BUDGET_USD"),
			value:      func(c *Config) string { return renderFloat(c.AggregateBudgetUSD) }},
		{name: "AggregateWindow", yamlKey: "aggregate_window", envVar: "DRYDOCK_AGGREGATE_WINDOW",
			guardedEnv: envDurationNonNegative("DRYDOCK_AGGREGATE_WINDOW"),
			value:      func(c *Config) string { return renderDur(c.AggregateWindow) }},
		{name: "PushMaxRetries", yamlKey: "push_max_retries", envVar: "DRYDOCK_PUSH_MAX_RETRIES",
			guardedEnv: envIntNonNegative("DRYDOCK_PUSH_MAX_RETRIES"),
			value:      func(c *Config) string { return renderInt(c.PushMaxRetries) }},
		{name: "PushRetryBackoff", yamlKey: "push_retry_backoff", envVar: "DRYDOCK_PUSH_RETRY_BACKOFF",
			guardedEnv: envDurationNonNegative("DRYDOCK_PUSH_RETRY_BACKOFF"),
			value:      func(c *Config) string { return renderDur(c.PushRetryBackoff) }},
		{name: "PushFreshBranchTries", yamlKey: "push_fresh_branch_tries", envVar: "DRYDOCK_PUSH_FRESH_BRANCH_TRIES",
			guardedEnv: envIntNonNegative("DRYDOCK_PUSH_FRESH_BRANCH_TRIES"),
			value:      func(c *Config) string { return renderInt(c.PushFreshBranchTries) }},
		{name: "OpenAICompat", yamlKey: "openai_compat",
			value: func(c *Config) string { return renderOpenAICompat(c.OpenAICompat) },
			// The rendered summary collapses base_path/api_key_env/model/prices,
			// so compare the struct fields against the (zero) default.
			differs: func(yamlCfg, def *Config) bool {
				return openAICompatShallowDiffers(yamlCfg.OpenAICompat, def.OpenAICompat)
			}},
		{name: "Verify.Repos", yamlKey: "verify.repos",
			value: func(c *Config) string { return renderVerifyRepos(c.Verify.Repos) },
			// Non-empty map = yaml-set (defaults have no repos).
			differs: func(yamlCfg, def *Config) bool {
				return len(yamlCfg.Verify.Repos) != len(def.Verify.Repos)
			}},
		{name: "Profiles", yamlKey: "profiles",
			value: func(c *Config) string { return renderProfiles(c.Profiles.Repos) },
			// Any repo configured = yaml-set (defaults have no repos).
			differs: func(yamlCfg, def *Config) bool {
				return len(yamlCfg.Profiles.Repos) != len(def.Profiles.Repos)
			}},
		{name: "DiffPolicy", yamlKey: "diff_policy",
			value: func(c *Config) string { return renderDiffPolicy(c.DiffPolicy) },
			// The rendered summary collapses the glob lists to counts, so
			// compare the struct itself: any non-zero field = yaml-set
			// (the default block is all-zero).
			differs: func(yamlCfg, def *Config) bool {
				return diffPolicySet(yamlCfg.DiffPolicy) != diffPolicySet(def.DiffPolicy)
			}},
		// ci: one row per field rather than a collapsed block summary. Each is
		// individually env-overridable, and ci.watch in particular decides
		// whether the host's gh credential is exercised on a timer — an
		// operator auditing that must see exactly which layer turned it on.
		{name: "CI.Watch", yamlKey: "ci.watch", envVar: "DRYDOCK_CI_WATCH",
			guardedEnv: envBoolOne("DRYDOCK_CI_WATCH"),
			value:      func(c *Config) string { return renderBool(c.CI.Watch) }},
		{name: "CI.PollInterval", yamlKey: "ci.poll_interval", envVar: "DRYDOCK_CI_POLL_INTERVAL",
			guardedEnv: envDurationNonNegative("DRYDOCK_CI_POLL_INTERVAL"),
			value:      func(c *Config) string { return renderDur(c.CI.PollInterval) }},
		{name: "CI.WatchTimeout", yamlKey: "ci.watch_timeout", envVar: "DRYDOCK_CI_WATCH_TIMEOUT",
			guardedEnv: envDurationNonNegative("DRYDOCK_CI_WATCH_TIMEOUT"),
			value:      func(c *Config) string { return renderDur(c.CI.WatchTimeout) }},
		{name: "CI.MaxAttempts", yamlKey: "ci.max_attempts", envVar: "DRYDOCK_CI_MAX_ATTEMPTS",
			guardedEnv: envIntNonNegative("DRYDOCK_CI_MAX_ATTEMPTS"),
			value:      func(c *Config) string { return renderInt(c.CI.MaxAttempts) }},
		{name: "StageRoot", yamlKey: "stage_root", envVar: "STAGE_ROOT",
			guardedEnv: envString("STAGE_ROOT"),
			value:      func(c *Config) string { return c.StageRoot }},
		{name: "AuditRoot", yamlKey: "audit_root", envVar: "AUDIT_ROOT",
			guardedEnv: envString("AUDIT_ROOT"),
			value:      func(c *Config) string { return c.AuditRoot }},
		{name: "SquidRunDir", yamlKey: "squid_run_dir", envVar: "SQUID_RUN_DIR",
			guardedEnv: envString("SQUID_RUN_DIR"),
			value:      func(c *Config) string { return c.SquidRunDir }},
		{name: "CacheRoot", yamlKey: "cache_root", envVar: "DRYDOCK_CACHE_ROOT",
			guardedEnv: envString("DRYDOCK_CACHE_ROOT"),
			value:      func(c *Config) string { return c.CacheRoot }},
		{name: "Broker.Socket", yamlKey: "broker.socket", envVar: "BROKER_SOCKET",
			guardedEnv: envString("BROKER_SOCKET"),
			value:      func(c *Config) string { return c.Broker.Socket }},
		{name: "Broker.Addr", yamlKey: "broker.addr", envVar: "BROKER_ADDR",
			guardedEnv: envString("BROKER_ADDR"),
			value:      func(c *Config) string { return c.Broker.Addr }},
		{name: "Notifications", yamlKey: "notifications", envVar: "DRYDOCK_NO_NOTIFY",
			guardedEnv: envBoolOne("DRYDOCK_NO_NOTIFY"),
			value:      func(c *Config) string { return renderBool(c.Notifications) }},
		{name: "LogJSON", yamlKey: "log_json", envVar: "DRYDOCK_LOG_JSON",
			guardedEnv: envBoolOne("DRYDOCK_LOG_JSON"),
			value:      func(c *Config) string { return renderBool(c.LogJSON) }},
		{name: "StrictContainerVersion", yamlKey: "strict_container_version", envVar: "DRYDOCK_STRICT_CONTAINER_VERSION",
			guardedEnv: envBoolOne("DRYDOCK_STRICT_CONTAINER_VERSION"),
			value:      func(c *Config) string { return renderBool(c.StrictContainerVersion) }},
	}
}

// openAICompatShallowDiffers compares the comparable scalars plus the prices
// map length (OpenAICompatConfig contains a map, so == is not available).
func openAICompatShallowDiffers(a, b OpenAICompatConfig) bool {
	if a.BaseURL != b.BaseURL || a.BasePath != b.BasePath ||
		a.APIKeyEnv != b.APIKeyEnv || a.Model != b.Model {
		return true
	}
	return len(a.Prices) != len(b.Prices)
}

// decodeYAMLOnly reads path into a fresh Defaults() copy WITHOUT applying env
// overrides — the yaml layer in isolation, for yaml-vs-default comparison.
// It mirrors Load's decode exactly: missing file is fine (pure defaults),
// KnownFields(true), single YAML document, tilde expansion. It does NOT
// validate: validation belongs to the fully-resolved config (env may repair a
// yaml value or vice versa), and Explain already runs Load for that.
func decodeYAMLOnly(path string) (*Config, error) {
	cfg := Defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			dec := yaml.NewDecoder(bytes.NewReader(b))
			dec.KnownFields(true)
			if err := dec.Decode(cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("parse %s: trailing YAML document; only one document is allowed", path)
			}
		case os.IsNotExist(err):
			// fine — defaults only
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	// Expand ~ like Load does, so a yaml path that expands to the default
	// (e.g. the seed template's ~/.drydock/stage) attributes to default,
	// not spuriously to config.yaml.
	cfg.expandHome()
	return cfg, nil
}

// Explain loads the config at path exactly like Load (missing file = defaults,
// validation runs) and additionally attributes every field's effective value
// to its source layer. Per field:
//  1. the field's env var is set AND passes the same guard applyEnvOverrides
//     uses → SourceEnv;
//  2. else the yaml file sets it to a non-default → SourceYAML;
//  3. else → SourceDefault.
//
// Step 1 re-runs the real guard, so an env var that is set but invalid (e.g.
// DRYDOCK_TASK_BUDGET_USD=-5, which fails the f>0 guard) correctly falls
// through to yaml/default instead of being misattributed to env.
func Explain(path string) ([]Field, *Config, error) {
	resolved, err := Load(path)
	if err != nil {
		return nil, nil, err
	}
	yamlCfg, err := decodeYAMLOnly(path)
	if err != nil {
		return nil, nil, err
	}
	def := Defaults()
	def.expandHome()

	descs := provenanceTable()
	fields := make([]Field, 0, len(descs))
	for _, d := range descs {
		src := SourceDefault
		switch {
		case d.envVar != "" && applies(d.guardedEnv):
			src = SourceEnv
		case yamlDiffers(d, yamlCfg, def):
			src = SourceYAML
		}
		fields = append(fields, Field{
			Name:    d.name,
			YAMLKey: d.yamlKey,
			EnvVar:  d.envVar,
			Value:   d.value(resolved),
			Source:  src,
		})
	}
	return fields, resolved, nil
}

func applies(guard func() (string, bool)) bool {
	if guard == nil {
		return false
	}
	_, ok := guard()
	return ok
}

// yamlDiffers reports whether the yaml layer sets d's field to a non-default,
// using the descriptor's custom comparison when the rendered value would
// collapse detail (maps/structs), else comparing rendered values.
func yamlDiffers(d fieldDesc, yamlCfg, def *Config) bool {
	if d.differs != nil {
		return d.differs(yamlCfg, def)
	}
	return d.value(yamlCfg) != d.value(def)
}

// PolicyComparisonFields returns fields minus the broker connection fields
// (Broker.Socket / Broker.Addr). Those two say how a client REACHES the
// daemon, not policy the daemon enforces — an operator dialing a remote
// daemon via BROKER_ADDR would otherwise trip a spurious DIVERGENT verdict.
// Both sides of the divergence comparison (the CLI's local hash and the
// daemon's boot-time PolicyHash) must route through this so the two hashes
// cover the identical field set. The full field list is still what
// /admin/policy serves and what the CLI table renders.
func PolicyComparisonFields(all []Field) []Field {
	out := make([]Field, 0, len(all))
	for _, f := range all {
		if f.Name == "Broker.Socket" || f.Name == "Broker.Addr" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// EffectiveHash is the divergence-comparison unit: sha256 (hex) over the
// canonical "Name=Value\n" lines sorted by Name. Two loads agree iff every
// field's effective value agrees, regardless of field ordering. Callers
// comparing local-vs-daemon policy hash PolicyComparisonFields(fields), not
// the full set. Each value is quoted to make it unambiguous (newlines in a
// Value cannot collide with field boundaries).
func EffectiveHash(fields []Field) string {
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		lines = append(lines, f.Name+"="+strconv.Quote(f.Value)+"\n")
	}
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(h[:])
}
