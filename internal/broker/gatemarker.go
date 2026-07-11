package broker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// gateMarker records a task blocked at the push-approval gate so a brokerd
// restart can resume it. Written when the gate is entered, removed when it
// resolves (approve/deny/kill/timeout), left in place on shutdown.
type gateMarker struct {
	RepoRef     string `json:"repo_ref"`
	Instruction string `json:"instruction"`
	Platform    string `json:"platform"`
	Agent       string `json:"agent"`
	Draft       bool   `json:"draft"`
	TaskStartMs int64  `json:"task_start_ms"`
}

func gateMarkerPath(auditRoot, id string) string {
	return filepath.Join(auditRoot, id+".gate.json")
}

func writeGateMarker(auditRoot, id string, m gateMarker) error {
	if err := os.MkdirAll(auditRoot, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(gateMarkerPath(auditRoot, id), payload, 0o600)
}

func readGateMarker(auditRoot, id string) (gateMarker, error) {
	var m gateMarker
	data, err := os.ReadFile(gateMarkerPath(auditRoot, id))
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
// Unreadable or malformed markers are skipped (logged by the caller if needed).
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
		if m, err := readGateMarker(auditRoot, id); err == nil {
			out[id] = m
		}
	}
	return out
}
