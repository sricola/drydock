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
	ID                        string
	MTime                     time.Time
	Outcome                   string // "ok"|"error"|"push_failed"|"interrupted"|"running"
	DurationMs                int64
	HasDuration               bool
	CostUSD                   float64
	Metered                   bool
	Agent, Vendor, Auth, Repo string
	HasMetrics                bool
	M                         audit.Metrics
}

// Summary is the aggregated view over a set of Samples.
type Summary struct {
	Tasks             int            `json:"tasks"`
	Outcomes          map[string]int `json:"outcomes"`
	DurP50Ms          int64          `json:"dur_p50_ms"`
	DurP95Ms          int64          `json:"dur_p95_ms"`
	EgressWaitP50Ms   int64          `json:"egress_wait_p50_ms"`
	EgressWaitP95Ms   int64          `json:"egress_wait_p95_ms"`
	ApprovalWaitP50Ms int64          `json:"approval_wait_p50_ms"`
	ApprovalWaitP95Ms int64          `json:"approval_wait_p95_ms"`
	SpendUSD          float64        `json:"spend_usd"`
	SpendPerDayUSD    float64        `json:"spend_per_day_usd"`
	UnmeteredTasks    int            `json:"unmetered_tasks"`
	Requests          int            `json:"requests"`
	WidenRequested    int            `json:"widen_requested"`
	WidenApproved     int            `json:"widen_approved"`
	PreMetricsTasks   int            `json:"pre_metrics_tasks"`
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

	last, hasResult := audit.LastResultFile(f)
	meta := audit.ReadMetaFile(f)
	m, hasMetrics := audit.LastMetricsFile(f)

	s := Sample{
		ID:          id,
		MTime:       mtime,
		Outcome:     outcomeFor(last, hasResult),
		DurationMs:  last.DurationMs,
		HasDuration: audit.HasDuration(last, hasResult),
		Metered:     !meta.Subscription,
		HasMetrics:  hasMetrics,
		M:           m,
	}
	if s.Metered {
		s.CostUSD = last.TotalCostUSD
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
		s.Agent = audit.TaskAgent(path)
		if v, ok := provider.VendorForAgent(s.Agent); ok {
			s.Vendor = v
		}
	}

	return s, true
}

// outcomeFor maps a Result to the stable outcome keys used in JSON output.
func outcomeFor(r audit.Result, ok bool) string {
	switch {
	case !ok:
		return "running"
	case r.Subtype == "interrupted":
		return "interrupted"
	case r.Subtype == "push_failed":
		return "push_failed"
	case r.IsError:
		return "error"
	default:
		return "ok"
	}
}

// Summarize aggregates samples into a Summary.
func Summarize(samples []Sample) Summary {
	s := Summary{Outcomes: map[string]int{}}
	s.Tasks = len(samples)

	var durs, egressWaits, approvalWaits []int64
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
	s.EgressWaitP50Ms = percentile(egressWaits, 50)
	s.EgressWaitP95Ms = percentile(egressWaits, 95)
	s.ApprovalWaitP50Ms = percentile(approvalWaits, 50)
	s.ApprovalWaitP95Ms = percentile(approvalWaits, 95)

	if s.Tasks > 0 {
		days := math.Ceil(newest.Sub(oldest).Hours() / 24)
		if days < 1 {
			days = 1
		}
		s.SpendPerDayUSD = s.SpendUSD / days
	}

	return s
}

// groupKeyFuncs maps a dimension name to a function extracting a sample's
// group key for that dimension.
var groupDims = []string{"agent", "vendor", "repo", "day", "week"}

func groupKey(sm Sample, dim string) (string, error) {
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
	default:
		return "", fmt.Errorf("unknown group dimension %q, want one of %s", dim, strings.Join(groupDims, "|"))
	}
	if key == "" {
		key = "(unknown)"
	}
	return key, nil
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
		key, err := groupKey(sm, dim)
		if err != nil {
			return nil, err
		}
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
