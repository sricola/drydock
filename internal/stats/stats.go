// Package stats aggregates drydock's per-task audit artifacts into run
// metrics: outcome rates, duration and gate-wait percentiles, spend, and
// egress-widen frequency. Read-only over the audit dir, brokerd not needed.
package stats

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"drydock/internal/audit"
	"drydock/internal/provider"
)

// Sample is one task's aggregated view, derived from its audit file (and, if
// present, its terminal metrics row).
type Sample struct {
	ID          string
	MTime       time.Time
	Outcome     string // "ok"|"error"|"push_failed"|"interrupted"|"running", or a passthrough subtype (e.g. "denied")
	DurationMs  int64
	HasDuration bool
	// CostUSD is BROKER-OBSERVED spend only: the src=="broker" result row's
	// total_cost_usd, which is the gateway lease's own figure. It is NEVER the
	// agent's self-reported number (G4) — an agent's stdout is untrusted, and
	// this value is summed into the spend total an operator reads.
	CostUSD float64
	// AgentReportedUSD is the figure the AGENT printed, kept only for traces
	// with no broker row yet (a running task, or one that ended before its
	// terminal row was written). It is reported SEPARATELY and never summed
	// into SpendUSD — but it is not thrown away either, because silently
	// showing $0 where a real number existed is its own dishonesty.
	AgentReportedUSD          float64
	HasAgentReportedUSD       bool
	Metered                   bool
	Agent, Vendor, Auth, Repo string
	HasMetrics                bool
	M                         audit.Metrics
}

// Summary is the aggregated view over a set of Samples.
type Summary struct {
	Tasks               int            `json:"tasks"`
	Outcomes            map[string]int `json:"outcomes"`
	DurP50Ms            int64          `json:"dur_p50_ms"`
	DurP95Ms            int64          `json:"dur_p95_ms"`
	DurSamples          int            `json:"dur_samples"`
	EgressWaitP50Ms     int64          `json:"egress_wait_p50_ms"`
	EgressWaitP95Ms     int64          `json:"egress_wait_p95_ms"`
	EgressWaitSamples   int            `json:"egress_wait_samples"`
	ApprovalWaitP50Ms   int64          `json:"approval_wait_p50_ms"`
	ApprovalWaitP95Ms   int64          `json:"approval_wait_p95_ms"`
	ApprovalWaitSamples int            `json:"approval_wait_samples"`
	QueueWaitP50Ms      int64          `json:"queue_wait_p50_ms"`
	QueueWaitP95Ms      int64          `json:"queue_wait_p95_ms"`
	QueueWaitSamples    int            `json:"queue_wait_samples"`
	// SpendUSD sums BROKER-OBSERVED cost only (G4). AgentReportedUSD is the
	// separate total of what agents claimed on tasks the broker never metered,
	// over AgentReportedTasks traces; it is reported so the number is visible,
	// and kept out of SpendUSD so it is never presented as measured spend.
	SpendUSD           float64 `json:"spend_usd"`
	SpendPerDayUSD     float64 `json:"spend_per_day_usd"`
	AgentReportedUSD   float64 `json:"agent_reported_usd"`
	AgentReportedTasks int     `json:"agent_reported_tasks"`
	UnmeteredTasks     int     `json:"unmetered_tasks"`
	Requests           int     `json:"requests"`
	WidenRequested     int     `json:"widen_requested"`
	WidenApproved      int     `json:"widen_approved"`
	PreMetricsTasks    int     `json:"pre_metrics_tasks"`
}

// Group is a Summary keyed by one value of a grouping dimension.
type Group struct {
	Key string `json:"key"`
	Summary
}

// Report is the top-level payload: an overall Summary plus optional groups.
type Report struct {
	Since        time.Time `json:"since"`
	Overall      Summary   `json:"overall"`
	Groups       []Group   `json:"groups,omitempty"`
	GroupBy      string    `json:"group_by,omitempty"`
	OrphanWidens int       `json:"orphan_widens"` // widen requested, task never ran
	SkippedFiles int       `json:"skipped_files"`
}

// Collect walks dir for *.jsonl audit files whose mtime is not before since (a
// zero since means no filter), turning each into a Sample. It also counts
// orphan *.widen.json files (widen requested but the task's .jsonl never
// existed) and files that could not be opened/stat'd. Returns samples,
// orphanWidens, skippedFiles.
func Collect(dir string, since time.Time) ([]Sample, int, int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, 0
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			present[strings.TrimSuffix(e.Name(), ".jsonl")] = true
		}
	}

	var samples []Sample
	skipped := 0
	orphans := 0

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			info, err := e.Info()
			if err != nil {
				skipped++
				continue
			}
			if !since.IsZero() && info.ModTime().Before(since) {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			path := filepath.Join(dir, name)
			s, ok := buildSample(path, id, info.ModTime())
			if !ok {
				skipped++
				continue
			}
			samples = append(samples, s)
		case strings.HasSuffix(name, ".widen.json"):
			info, err := e.Info()
			if err != nil {
				skipped++
				continue
			}
			if !since.IsZero() && info.ModTime().Before(since) {
				continue
			}
			id := strings.TrimSuffix(name, ".widen.json")
			if !present[id] {
				orphans++
			}
		}
	}

	return samples, orphans, skipped
}

// buildSample reads one audit file into a Sample. ok=false when the file
// cannot be opened at all (truly unreadable, counted as skipped by Collect).
func buildSample(path, id string, mtime time.Time) (Sample, bool) {
	f, err := audit.OpenRead(path)
	if err != nil {
		return Sample{}, false
	}
	defer f.Close()

	// One tail read for every terminal row, including the src=="broker" result
	// row the spend figure must come from.
	rows := audit.LastRowsFile(f)
	last, hasResult := rows.Result, rows.HasResult
	m, hasMetrics := rows.Metrics, rows.HasMetrics
	meta := audit.ReadMetaFile(f)

	s := Sample{
		ID:          id,
		MTime:       mtime,
		Outcome:     audit.OutcomeKeyWithMetrics(last, hasResult, m, hasMetrics),
		DurationMs:  last.DurationMs,
		HasDuration: audit.HasDuration(last, hasResult),
		Metered:     !meta.Subscription,
		HasMetrics:  hasMetrics,
		M:           m,
	}
	// G4: the spend total `drydock stats` prints is BROKER-OBSERVED. This used
	// to read the last result row of any src, so an agent that printed
	// {"type":"result","total_cost_usd":9999} moved the operator's spend total.
	// Only the src=="broker" row counts now; an agent-only figure is carried
	// separately and rendered as agent-reported rather than dropped.
	if s.Metered {
		switch {
		case rows.HasBroker:
			s.CostUSD = rows.Broker.TotalCostUSD
		case hasResult:
			s.AgentReportedUSD, s.HasAgentReportedUSD = last.TotalCostUSD, true
		}
	}
	if meta.Subscription {
		s.Auth = "subscription"
	} else {
		s.Auth = "api_key"
	}

	if hasMetrics {
		s.Agent = m.Agent
		s.Vendor = m.Vendor
		if m.Auth != "" {
			s.Auth = m.Auth
		}
		s.Repo = m.Repo
	} else {
		s.Agent = audit.TaskAgentFile(f)
		if v, ok := provider.VendorForAgent(s.Agent); ok {
			s.Vendor = v
		}
	}

	return s, true
}

// Summarize aggregates samples into a Summary.
func Summarize(samples []Sample) Summary {
	s := Summary{Outcomes: map[string]int{}}
	s.Tasks = len(samples)

	var durs, egressWaits, approvalWaits, queueWaits []int64
	var oldest, newest time.Time

	for i, sm := range samples {
		s.Outcomes[sm.Outcome]++

		if sm.HasDuration {
			durs = append(durs, sm.DurationMs)
		}

		if sm.HasMetrics {
			if sm.M.EgressGateWaitMs > 0 {
				egressWaits = append(egressWaits, sm.M.EgressGateWaitMs)
			}
			if sm.M.ApprovalGateWaitMs > 0 {
				approvalWaits = append(approvalWaits, sm.M.ApprovalGateWaitMs)
			}
			// stage_ms.queued is present only on tasks that came through the
			// durable queue; synchronous tasks are excluded from the sample
			// set (same rule as the gate waits) rather than dragging the
			// queue-latency percentiles toward zero.
			if sm.M.StageMs.Queued > 0 {
				queueWaits = append(queueWaits, sm.M.StageMs.Queued)
			}
			s.Requests += sm.M.Requests
			s.WidenRequested += sm.M.WidenRequested
			if sm.M.WidenOutcome == "approved" {
				s.WidenApproved++
			}
		} else {
			s.PreMetricsTasks++
		}

		if sm.Metered {
			s.SpendUSD += sm.CostUSD
			if sm.HasAgentReportedUSD {
				s.AgentReportedUSD += sm.AgentReportedUSD
				s.AgentReportedTasks++
			}
		} else {
			s.UnmeteredTasks++
		}

		if i == 0 || sm.MTime.Before(oldest) {
			oldest = sm.MTime
		}
		if i == 0 || sm.MTime.After(newest) {
			newest = sm.MTime
		}
	}

	s.DurP50Ms = percentile(durs, 50)
	s.DurP95Ms = percentile(durs, 95)
	s.DurSamples = len(durs)
	s.EgressWaitP50Ms = percentile(egressWaits, 50)
	s.EgressWaitP95Ms = percentile(egressWaits, 95)
	s.EgressWaitSamples = len(egressWaits)
	s.ApprovalWaitP50Ms = percentile(approvalWaits, 50)
	s.ApprovalWaitP95Ms = percentile(approvalWaits, 95)
	s.ApprovalWaitSamples = len(approvalWaits)
	s.QueueWaitP50Ms = percentile(queueWaits, 50)
	s.QueueWaitP95Ms = percentile(queueWaits, 95)
	s.QueueWaitSamples = len(queueWaits)

	if s.Tasks > 0 {
		days := math.Ceil(newest.Sub(oldest).Hours() / 24)
		if days < 1 {
			days = 1
		}
		s.SpendPerDayUSD = s.SpendUSD / days
	}

	return s
}

// groupDims lists the valid dimensions accepted by GroupBy.
var groupDims = []string{"agent", "vendor", "repo", "day", "week"}

// groupKey extracts sm's group key for dim. Callers must validate dim
// against groupDims first (GroupBy does); dim is assumed valid here.
func groupKey(sm Sample, dim string) string {
	var key string
	switch dim {
	case "agent":
		key = sm.Agent
	case "vendor":
		key = sm.Vendor
	case "repo":
		key = sm.Repo
	case "day":
		key = sm.MTime.Format("2006-01-02")
	case "week":
		y, w := sm.MTime.ISOWeek()
		key = fmt.Sprintf("%d-W%02d", y, w)
	}
	if key == "" {
		key = "(unknown)"
	}
	return key
}

// GroupBy partitions samples by dim ("agent"|"vendor"|"repo"|"day"|"week"),
// returning one Group per distinct key, sorted by key ascending. dim is
// validated even when samples is empty.
func GroupBy(samples []Sample, dim string) ([]Group, error) {
	found := false
	for _, d := range groupDims {
		if d == dim {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown group dimension %q, want one of %s", dim, strings.Join(groupDims, "|"))
	}

	buckets := map[string][]Sample{}
	for _, sm := range samples {
		key := groupKey(sm, dim)
		buckets[key] = append(buckets[key], sm)
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	groups := make([]Group, 0, len(keys))
	for _, k := range keys {
		groups = append(groups, Group{Key: k, Summary: Summarize(buckets[k])})
	}
	return groups, nil
}

// percentile returns the p-th percentile (0-100) of vals using the
// nearest-rank method. Empty input returns 0.
func percentile(vals []int64, p float64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx > len(sorted)-1 {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
