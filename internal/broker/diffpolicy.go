package broker

import (
	"fmt"
	"sort"

	"drydock/internal/config"
	"drydock/internal/globmatch"
	"drydock/internal/trustbrief"
)

// requiredAcks computes the second-look acknowledgment categories for a diff:
// the sorted, de-duplicated set of flag kinds (trustbrief.Flag.Kind) where ANY
// of the flag's example paths matches ANY diff_policy.second_look_paths glob.
// Empty SecondLookPaths means the feature is off — nil, no acks ever required.
//
// Scope note (deliberate, security-reviewed): second-look is a HUMAN-GATE
// REVIEW AID, not a containment boundary. The hard enforcement layer for
// untouchable or unevaluatable diffs is checkDiffCaps, which fails closed
// (policy_blocked) when FilesOmitted > 0 and a content-based policy
// (blocked_paths / max_lines_changed) is configured. second_look_paths alone
// deliberately does NOT trigger that guard: the approver still sees the full
// diff and the Brief's files_omitted count at review. So when files were
// omitted from analysis (past trustbrief's tracking bound), acks are computed
// over the tracked files only — omitted files simply can't add categories.
// Flag example paths are themselves capped (trustbrief maxFlagPaths), which is
// the same best-effort character. Anything that must be impossible to push
// belongs in blocked_paths, not here.
func requiredAcks(facts trustbrief.DiffFacts, dp config.DiffPolicy) []string {
	if len(dp.SecondLookPaths) == 0 {
		return nil
	}
	kinds := map[string]bool{}
	for _, fl := range facts.Flags {
		for _, p := range fl.Paths {
			for _, pat := range dp.SecondLookPaths {
				if globmatch.Match(pat, p) {
					kinds[fl.Kind] = true
				}
			}
		}
	}
	if len(kinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkDiffCaps applies the operator's diff_policy caps (Broker.DiffPolicy)
// to the broker-computed DiffFacts for this task's review diff. It returns
// blocked=true with a human-readable reason naming the specific violation:
// which cap was exceeded and its configured value, or which path matched
// which blocked_paths pattern. A zero-value policy blocks nothing.
//
// The facts come from the same trustbrief.Analyze pass writeBrief persisted,
// so the count-based caps enforce exactly what the Brief reports.
// FilesOmitted counts toward the file cap: files past Analyze's tracking
// bound are still changed files, and an attacker must not escape the cap by
// exceeding the parser's retention limit. Omitted files carry no per-file
// line counts, so when any file was omitted and a content-based policy
// (blocked_paths or max_lines_changed) is configured, the diff cannot be
// fully evaluated and the task fails closed rather than letting an untracked
// file slip past the policy. The max_files_changed check runs first so an
// over-cap diff reports the clearer count-based reason.
//
// blocked_paths deliberately does NOT match against facts.Files: those paths
// are display-capped ADVISORY evidence (truncated to trustbrief's stored
// path bound, "" for an over-long header) and a containment decision matched
// against them is bypassable — a blocked file at a path longer than the cap
// no longer matches its glob, and a 100%-similarity rename records only the
// destination so a rename OUT of a blocked directory escapes. The check
// instead re-derives the complete, uncapped path set from the raw diff via
// trustbrief.ChangedPathsForPolicy (which also includes rename sources) and
// fails closed whenever that scan reports the set may be incomplete. The
// FilesOmitted guard above is kept as defense in depth.
func (tr *taskRun) checkDiffCaps(facts trustbrief.DiffFacts, diff string) (blocked bool, reason string) {
	p := tr.b.DiffPolicy
	if files := len(facts.Files) + facts.FilesOmitted; p.MaxFilesChanged > 0 && files > p.MaxFilesChanged {
		return true, fmt.Sprintf("diff changes %d files, over the diff_policy.max_files_changed cap of %d",
			files, p.MaxFilesChanged)
	}
	if facts.FilesOmitted > 0 && (len(p.BlockedPaths) > 0 || p.MaxLinesChanged > 0) {
		return true, fmt.Sprintf("diff too large to fully evaluate against policy: %d files omitted from analysis; refusing to approve a blocked_paths/max_lines policy against an incompletely-analyzed diff",
			facts.FilesOmitted)
	}
	if p.MaxLinesChanged > 0 {
		lines := 0
		for _, f := range facts.Files {
			lines += f.Adds + f.Dels
		}
		if lines > p.MaxLinesChanged {
			return true, fmt.Sprintf("diff changes %d lines (added+deleted), over the diff_policy.max_lines_changed cap of %d",
				lines, p.MaxLinesChanged)
		}
	}
	if len(p.BlockedPaths) > 0 {
		paths, complete := trustbrief.ChangedPathsForPolicy(diff)
		if !complete {
			return true, "diff contains paths too long or malformed to fully evaluate against blocked_paths policy"
		}
		for _, fp := range paths {
			for _, pat := range p.BlockedPaths {
				if globmatch.Match(pat, fp) {
					return true, fmt.Sprintf("diff touches blocked path %s (matches diff_policy.blocked_paths pattern %q)",
						fp, pat)
				}
			}
		}
	}
	return false, ""
}
