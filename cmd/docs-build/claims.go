// The security-defaults table (F-10): one generated source of truth for the
// operator-facing financial and containment defaults. Values come from
// config.Defaults() and exported constants, never from prose, so the table
// cannot drift from the code; TestSecurityDefaultsPageCurrent pins the
// committed copy and TestSecurityDefaultsVerifiedByTestsExist pins each
// row's enforcing test.
package main

import (
	"fmt"
	"strings"

	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/stage"
)

type claim struct {
	Setting string // config key, or (built-in) for compiled constants
	Default string // rendered from code
	Bounds  string // what it bounds, and whether the bound is hard or soft
	Test    string // the Go test enforcing it (existence is itself tested)
}

func securityClaims() []claim {
	d := config.Defaults()
	return []claim{
		{"task_budget_usd", fmt.Sprintf("%.2f", d.TaskBudgetUSD),
			"Per-task USD spend through the gateway. Soft: metering is post-hoc, so spend can overshoot by up to task_max_inflight in-flight requests.",
			"TestGateway_OverBudget402"},
		{"task_max_inflight", fmt.Sprintf("%d", d.TaskMaxInFlight),
			"Concurrent gateway requests per task lease. Hard at admission; bounds the budget overshoot.",
			"TestAdmit_InFlightLimit"},
		{"task_max_requests", fmt.Sprintf("%d (0 falls closed to %d)", d.TaskMaxRequests, broker.DefaultUncappedRequestCap),
			"Total gateway requests per task, every auth mode. Hard at admission.",
			"TestRequestCap_RejectsOverLimit"},
		{"max_request_cost_usd", fmt.Sprintf("%.2f (0 = reservation off)", d.MaxRequestCostUSD),
			"Per-request USD reservation taken at admission. Off by default; setting it makes the per-task budget reservation-backed.",
			"TestAdmit_InFlightReservationBounds"},
		{"aggregate_budget_usd", fmt.Sprintf("%.2f (0 = off)", d.AggregateBudgetUSD),
			"Cross-task USD ceiling per api_key vendor over aggregate_window. Soft in the same post-hoc sense as task_budget_usd.",
			"TestGateway_AggregateCap"},
		{"task_timeout", d.TaskTimeout.String(),
			"Wall-clock bound per task: on expiry the run context is cancelled and the task terminates without pushing (VM force-delete is best effort). Hard.",
			"TestHandleTask_TimeoutTerminatesAndDoesNotPush"},
		{"verify.repos.*.timeout", fmt.Sprintf("%s per command when the repo's config leaves it unset", broker.DefaultVerifyTimeout),
			"Wall-clock bound per verification command; on expiry the verify VM is force-removed and the command records timed_out, so the overall verdict reads inconclusive — never passed. Verifier output is display-only (log + host-side digest), bounded by the same per-task host output cap limit as the agent run, from a separate budget. Hard.",
			"TestRunVerify_TimeoutIsInconclusiveNeverPassed"},
		{"profiles.repos.*.timeout", fmt.Sprintf("%s per command when the repo's profile leaves it unset", broker.DefaultSetupTimeout),
			"Wall-clock bound per setup/readiness command; on expiry the setup VM is force-removed, the command records timed_out, and the task fails closed with outcome setup_failed BEFORE the agent VM boots — no credential is ever injected into any VM and no API budget is spent. Setup output is display-only (log + host-side digest), bounded by the same per-task host output cap as the agent run. Hard.",
			"TestRunSetup_TimeoutForceDeletesVM"},
		{"stage_quota_gb", fmt.Sprintf("%d", d.StageQuotaGB),
			"Per-task disk bound: the stage dir is an APFS sparse image of this size (macOS). Hard (filesystem ENOSPC).",
			"TestQuota_HardBoundENOSPC"},
		{"(built-in) stage soft bounds", fmt.Sprintf("%d GiB, %d files, %d GiB host free floor",
			broker.DefaultMaxStageBytes>>30, broker.DefaultMaxStageFiles, broker.DefaultMinFreeStageBytes>>30),
			"Polling guard (2s) cancels a task growing past these, before the hard quota wall. Soft by design; the quota is the wall.",
			"TestHandleTask_StageFillTerminatesAndDoesNotPush"},
		{"cache_quota_gb", fmt.Sprintf("%d (0 = cache disabled)", d.CacheQuotaGB),
			"Total disk for the persistent dependency cache under cache_root, all opted-in repos combined. Least-recently-used entries are evicted past the bound (and below a host free-space floor) at brokerd boot and after each cache-using task; entries mounted by live tasks are never evicted, so a sweep can end over-quota only while those tasks still hold the excess.",
			"TestStore_EvictLRU"},
		{"(built-in) review diff cap", fmt.Sprintf("%d MiB", int64(stage.MaxDiffBytes)>>20),
			"A staged diff over the cap fails the task closed; a diff is never truncated for review. Hard.",
			"TestCaptureDiff_OversizeDiffFailsClosed"},
	}
}

// renderClaims renders the table as the full Markdown page. Deterministic
// output: the committed copy is byte-compared by the drift test.
func renderClaims(claims []claim) string {
	var b strings.Builder
	b.WriteString("# Security defaults\n\n")
	b.WriteString("What the shipped defaults bound, how hard each bound is, and the test that enforces it. ")
	b.WriteString("Generated from `config.Defaults()` and the exported constants, so this page cannot drift from the code.\n\n")
	b.WriteString("<!-- GENERATED by cmd/docs-build (claims.go); do not edit by hand. Regenerate with `go run ./cmd/docs-build`. -->\n\n")
	b.WriteString("| Setting | Default | What it bounds | Verified by |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, c := range claims {
		fmt.Fprintf(&b, "| `%s` | %s | %s | `%s` |\n", c.Setting, c.Default, c.Bounds, c.Test)
	}
	b.WriteString("\nSoft means enforcement is post-hoc or polling-based with a stated overshoot bound; hard means the mechanism cannot be raced. ")
	b.WriteString("The full adversarial context is in the [threat model](threat-model.html).\n")
	return b.String()
}
