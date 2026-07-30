package broker

import (
	"fmt"

	"drydock/internal/globmatch"
	"drydock/internal/trustbrief"
)

// checkDiffCaps applies the operator's diff_policy caps (Broker.DiffPolicy)
// to the broker-computed DiffFacts for this task's review diff. It returns
// blocked=true with a human-readable reason naming the specific violation:
// which cap was exceeded and its configured value, or which path matched
// which blocked_paths pattern. A zero-value policy blocks nothing.
//
// The facts come from the same trustbrief.Analyze pass writeBrief persisted,
// so what the caps enforce is exactly what the Brief reports. FilesOmitted
// counts toward the file cap: files past Analyze's tracking bound are still
// changed files, and an attacker must not escape the cap by exceeding the
// parser's retention limit. Omitted files carry no per-file line counts or
// paths, so the line and blocked-path checks can only see the tracked files —
// when any file was omitted and a content-based policy (blocked_paths or
// max_lines_changed) is configured, the diff cannot be fully evaluated and
// the task fails closed rather than letting an untracked file slip past the
// policy. The max_files_changed check runs first so an over-cap diff reports
// the clearer count-based reason.
func (tr *taskRun) checkDiffCaps(facts trustbrief.DiffFacts) (blocked bool, reason string) {
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
	for _, f := range facts.Files {
		for _, pat := range p.BlockedPaths {
			if globmatch.Match(pat, f.Path) {
				return true, fmt.Sprintf("diff touches blocked path %s (matches diff_policy.blocked_paths pattern %q)",
					f.Path, pat)
			}
		}
	}
	return false, ""
}
