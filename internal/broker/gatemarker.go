package broker

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"drydock/internal/atomicfile"
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

func gateMarkerPath(auditRoot, id string) string {
	return filepath.Join(auditRoot, id+".gate.json")
}

// writeGateMarker persists the marker atomically (temp + fsync + rename), the
// same guarantee the queue and CI marker stores give: a crash mid-write must
// never leave a torn marker, because a marker that fails to parse at the next
// boot means the task is not resumed and its stage is reaped.
func writeGateMarker(auditRoot, id string, m gateMarker) error {
	if err := os.MkdirAll(auditRoot, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(gateMarkerPath(auditRoot, id), payload, 0o600)
}

// readGateMarker refuses a symlinked marker (O_NOFOLLOW), matching every other
// read in the audit dir, so a planted <id>.gate.json -> elsewhere cannot feed
// the resumed gate a substituted repo or instruction.
func readGateMarker(auditRoot, id string) (gateMarker, error) {
	var m gateMarker
	f, err := os.OpenFile(gateMarkerPath(auditRoot, id), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return m, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

func removeGateMarker(auditRoot, id string) error {
	err := os.Remove(gateMarkerPath(auditRoot, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ListGateMarkers returns every task id with a live gate marker in auditRoot.
// An unreadable or malformed marker is skipped with a warning: the task it
// named will not be resumed, and that should be visible in the boot log
// rather than silent.
func ListGateMarkers(auditRoot string) map[string]gateMarker {
	out := map[string]gateMarker{}
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		return out
	}
	const suffix = ".gate.json"
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), suffix)
		m, err := readGateMarker(auditRoot, id)
		if err != nil {
			slog.Warn("resume: skipping unreadable gate marker; its task will not be resumed", "file", e.Name(), "err", err)
			continue
		}
		out[id] = m
	}
	return out
}
