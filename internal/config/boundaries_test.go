package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const boundaryBase = "network: x\ngateway_ip: 1.2.3.4\n"

func loadYAML(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(boundaryBase+yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// The exact edge of every numeric bound: the smallest legal value loads, the
// first illegal value is refused naming the field. (Mutation testing showed the
// off-by-one direction of several of these comparisons was unpinned.)
func TestValidate_ExactBoundaries(t *testing.T) {
	cases := []struct {
		yaml  string
		ok    bool
		field string
	}{
		{"max_concurrent_tasks: 1\n", true, "max_concurrent_tasks"},
		{"max_concurrent_tasks: 0\n", false, "max_concurrent_tasks"},
		{"task_timeout: 1s\n", true, "task_timeout"},
		{"task_timeout: 999ms\n", false, "task_timeout"},
		{"approval_timeout: 0s\n", true, "approval_timeout"},
		{"approval_timeout: 1s\n", true, "approval_timeout"},
		{"approval_timeout: 999ms\n", false, "approval_timeout"},
		{"task_max_requests: 0\n", true, "task_max_requests"},
		{"task_max_inflight: 0\n", true, "task_max_inflight"},
		{"task_max_inflight: -1\n", false, "task_max_inflight"},
		{"stage_quota_gb: 0\n", true, "stage_quota_gb"},
		{"stage_quota_gb: -1\n", false, "stage_quota_gb"},
		{"cache_quota_gb: 0\n", true, "cache_quota_gb"},
		{"cache_quota_gb: -1\n", false, "cache_quota_gb"},
		{"aggregate_window: 0s\n", true, "aggregate_window"},
		{"aggregate_window: -1s\n", false, "aggregate_window"},
		{"push_max_retries: 0\n", true, "push_max_retries"},
		{"push_max_retries: -1\n", false, "push_max_retries"},
		{"push_retry_backoff: 0s\n", true, "push_retry_backoff"},
		{"push_retry_backoff: -1s\n", false, "push_retry_backoff"},
		{"push_fresh_branch_tries: 0\n", true, "push_fresh_branch_tries"},
		{"push_fresh_branch_tries: -1\n", false, "push_fresh_branch_tries"},
		{"task_budget_usd: 0.000001\n", true, "task_budget_usd"},
		{"task_budget_usd: 0\n", false, "task_budget_usd"},
	}
	for _, c := range cases {
		_, err := loadYAML(t, c.yaml)
		if c.ok && err != nil {
			t.Errorf("%q: want accepted, got %v", strings.TrimSpace(c.yaml), err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), c.field)) {
			t.Errorf("%q: want an error naming %s, got %v", strings.TrimSpace(c.yaml), c.field, err)
		}
	}
}

// Env overrides: 0 is a real value where 0 is legal (it means "unlimited" or
// "off") and must win over the file; where 0 is illegal the env value is
// ignored and the file value stands.
func TestEnvOverride_ZeroSemantics(t *testing.T) {
	file := "task_max_requests: 5\ntask_max_inflight: 3\nstage_quota_gb: 4\ncache_quota_gb: 9\n" +
		"push_max_retries: 2\npush_fresh_branch_tries: 2\npush_retry_backoff: 2s\naggregate_window: 2h\n" +
		"queue:\n  max_denials: 3\ntask_budget_usd: 1.5\nmax_concurrent_tasks: 4\n"
	for _, kv := range [][2]string{
		{"DRYDOCK_TASK_MAX_REQUESTS", "0"}, {"DRYDOCK_TASK_MAX_INFLIGHT", "0"},
		{"DRYDOCK_STAGE_QUOTA_GB", "0"}, {"DRYDOCK_CACHE_QUOTA_GB", "0"},
		{"DRYDOCK_PUSH_MAX_RETRIES", "0"}, {"DRYDOCK_PUSH_FRESH_BRANCH_TRIES", "0"},
		{"DRYDOCK_PUSH_RETRY_BACKOFF", "0s"}, {"DRYDOCK_AGGREGATE_WINDOW", "0s"},
		{"DRYDOCK_QUEUE_MAX_DENIALS", "0"},
		{"DRYDOCK_TASK_BUDGET_USD", "0"}, {"DRYDOCK_MAX_CONCURRENT_TASKS", "0"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := loadYAML(t, file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	zero := map[string]int{
		"task_max_requests": c.TaskMaxRequests, "task_max_inflight": c.TaskMaxInFlight,
		"stage_quota_gb": c.StageQuotaGB, "cache_quota_gb": c.CacheQuotaGB,
		"push_max_retries": c.PushMaxRetries, "push_fresh_branch_tries": c.PushFreshBranchTries,
		"queue.max_denials": c.Queue.MaxDenials,
	}
	for k, v := range zero {
		if v != 0 {
			t.Errorf("%s = %d; env 0 must override the file", k, v)
		}
	}
	if c.PushRetryBackoff != 0 || c.AggregateWindow != 0 {
		t.Errorf("durations: backoff %v window %v; env 0s must override the file", c.PushRetryBackoff, c.AggregateWindow)
	}
	if c.TaskBudgetUSD != 1.5 {
		t.Errorf("task_budget_usd = %v; an illegal env 0 must be ignored", c.TaskBudgetUSD)
	}
	if c.MaxConcurrent != 4 {
		t.Errorf("max_concurrent_tasks = %v; an illegal env 0 must be ignored", c.MaxConcurrent)
	}
}

// More env knobs where 0 is legal and must win over the file.
func TestEnvOverride_ZeroSemantics_SpendAndCIKnobs(t *testing.T) {
	file := "max_request_cost_usd: 0.5\naggregate_budget_usd: 9\nglobal_budget_usd: 9\nglobal_max_tasks: 9\nglobal_window: 2h\n" +
		"ci:\n  max_attempts: 2\n  poll_interval: 30s\napproval_timeout: 1h\n"
	for _, kv := range [][2]string{
		{"DRYDOCK_MAX_REQUEST_COST_USD", "0"}, {"DRYDOCK_AGGREGATE_BUDGET_USD", "0"},
		{"DRYDOCK_GLOBAL_BUDGET_USD", "0"}, {"DRYDOCK_GLOBAL_MAX_TASKS", "0"},
		{"DRYDOCK_GLOBAL_WINDOW", "0s"}, {"DRYDOCK_CI_MAX_ATTEMPTS", "0"}, {"DRYDOCK_CI_POLL_INTERVAL", "0s"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	c, err := loadYAML(t, file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxRequestCostUSD != 0 || c.AggregateBudgetUSD != 0 || c.GlobalBudgetUSD != 0 || c.GlobalMaxTasks != 0 || c.GlobalWindow != 0 {
		t.Errorf("spend knobs not zeroed by env: cost %v agg %v global %v/%d window %v",
			c.MaxRequestCostUSD, c.AggregateBudgetUSD, c.GlobalBudgetUSD, c.GlobalMaxTasks, c.GlobalWindow)
	}
	if c.CI.MaxAttempts != 0 || c.CI.PollInterval != 0 {
		t.Errorf("ci knobs not zeroed by env: attempts %d poll %v", c.CI.MaxAttempts, c.CI.PollInterval)
	}
}

// A zero price and a zero verify timeout are legal (free local models; "no
// timeout" falls to the default); only negatives are refused.
func TestValidate_ZeroPriceAndZeroVerifyTimeoutAccepted(t *testing.T) {
	oc := "openai_compat:\n  base_url: https://llm.example\n  api_key_env: K\n  model: m\n  prices:\n    m: {input: 0, output: 0}\n"
	if _, err := loadYAML(t, oc); err != nil {
		t.Errorf("zero price rejected: %v", err)
	}
	neg := "openai_compat:\n  base_url: https://llm.example\n  api_key_env: K\n  model: m\n  prices:\n    m: {input: -0.1, output: 0}\n"
	if _, err := loadYAML(t, neg); err == nil || !strings.Contains(err.Error(), "price") {
		t.Errorf("negative price: want an error naming price, got %v", err)
	}
	vr := "verify:\n  repos:\n    github.com/o/r:\n      timeout: 0s\n      commands: [[\"true\"]]\n"
	if _, err := loadYAML(t, vr); err != nil {
		t.Errorf("verify timeout 0s rejected: %v", err)
	}
}

// ciDispatchFloor: a non-positive poll uses the default; the floor is
// max(CIDispatchFloorPolls*poll, CIDispatchFloorGrace), with equality
// resolving to the grace value.
func TestCIDispatchFloor_Boundaries(t *testing.T) {
	if got, want := ciDispatchFloor(0), ciDispatchFloor(DefaultCIPollInterval); got != want {
		t.Errorf("floor(0) = %v, want floor(default poll) = %v", got, want)
	}
	if got := ciDispatchFloor(-time.Second); got != ciDispatchFloor(DefaultCIPollInterval) {
		t.Errorf("floor(-1s) = %v, want the default-poll floor", got)
	}
	polls := time.Duration(CIDispatchFloorPolls)
	atGrace := CIDispatchFloorGrace / polls
	if got := ciDispatchFloor(atGrace); got != CIDispatchFloorGrace {
		t.Errorf("floor(%v) = %v, want exactly the grace %v", atGrace, got, CIDispatchFloorGrace)
	}
	above := atGrace + time.Second
	if got := ciDispatchFloor(above); got != polls*above {
		t.Errorf("floor(%v) = %v, want %d*poll = %v", above, got, CIDispatchFloorPolls, polls*above)
	}
	below := atGrace - time.Second
	if got := ciDispatchFloor(below); got != CIDispatchFloorGrace {
		t.Errorf("floor(%v) = %v, want the grace %v", below, got, CIDispatchFloorGrace)
	}
}
