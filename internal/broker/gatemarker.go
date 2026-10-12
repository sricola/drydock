package broker

import (
	"log/slog"
	"path/filepath"
)

// gateMarker records a task blocked at the push-approval gate so a brokerd
// restart can resume it. Written when the gate is entered, removed when it
// resolves (approve/deny/kill/timeout), left in place on shutdown.
type gateMarker struct {
	RepoRef     string `json:"repo_ref"`
	Instruction string `json:"instruction"`
	// RootInstruction and IssueURL let a task resumed after a restart compute
	// the same rejection keys the live path did. omitempty: markers from older
	// builds lack them, and the resume path then keys on Instruction alone,
	// which is right for every non-retry, non-issue task.
	RootInstruction string `json:"root_instruction,omitempty"`
	IssueURL        string `json:"issue_url,omitempty"`
	Platform        string `json:"platform"`
	Agent           string `json:"agent"`
	Draft           bool   `json:"draft"`
	TaskStartMs     int64  `json:"task_start_ms"`
}

const gateSuffix = ".gate.json"

var gateStore = markerStore[gateMarker]{suffix: gateSuffix, what: "gatemarker"}

func gateMarkerPath(auditRoot, id string) string {
	return filepath.Join(auditRoot, id+gateSuffix)
}

// writeGateMarker persists the marker through markerStore (id validated,
// atomic fsynced write, 0600): a crash mid-write must never leave a torn
// marker, because a marker that fails to parse at the next boot means the
// task is not resumed and its stage is reaped.
func writeGateMarker(auditRoot, id string, m gateMarker) error {
	return gateStore.write(auditRoot, id, m)
}

// readGateMarker refuses a symlinked marker (O_NOFOLLOW), so a planted
// <id>.gate.json -> elsewhere cannot feed the resumed gate a substituted repo
// or instruction.
func readGateMarker(auditRoot, id string) (gateMarker, error) {
	return gateStore.read(auditRoot, id)
}

func removeGateMarker(auditRoot, id string) error {
	return gateStore.remove(auditRoot, id)
}

// ListGateMarkers returns every task id with a live gate marker in auditRoot.
// An unreadable or malformed marker is skipped with a warning: the task it
// named will not be resumed, and that should be visible in the boot log
// rather than silent. A missing audit root is an empty listing.
func ListGateMarkers(auditRoot string) map[string]gateMarker {
	out, err := gateStore.list(auditRoot)
	if err != nil {
		slog.Warn("gatemarker: could not scan the audit dir for gate markers", "audit_root", auditRoot, "err", err)
	}
	return out
}
