package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// handleSubmit starts a task. The UI server — not the browser — owns the brokerd
// connection, but because brokerd roots task context at Background (see the
// broker detach change), the server can read the `accepted` line for the id and
// close immediately; the task runs independently and is observed via polling.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Refuse auto_approve: the entire point of the UI is a human at the gate.
	var probe struct {
		AutoApprove bool `json:"auto_approve"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.AutoApprove {
		http.Error(w, "auto_approve is not allowed from the web UI; approve at the gate", http.StatusBadRequest)
		return
	}

	// Use the cached no-timeout client (built once in Handler). Background
	// context: tasks run for minutes and must not die when this handler returns.
	if s.brokerNoTimeout == nil {
		http.Error(w, "brokerd not running — run `drydock start`", http.StatusBadGateway)
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", s.brokerBase+"/tasks", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.brokerNoTimeout.Do(req)
	if err != nil {
		http.Error(w, "brokerd not running — run `drydock start`", http.StatusBadGateway)
		return
	}
	// Pre-accept failures (bad repo 400, slot full 503, bad egress 400) are plain
	// non-200 bodies with NO stream — surface verbatim and stop. No goroutine.
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
		return
	}
	// 200: the first NDJSON line is the accepted event carrying the task id.
	line, err := bufio.NewReader(resp.Body).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		resp.Body.Close()
		http.Error(w, "brokerd accepted no task", http.StatusBadGateway)
		return
	}
	var ev struct {
		Event  string `json:"event"`
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil || ev.Event != "accepted" || ev.TaskID == "" {
		resp.Body.Close()
		w.WriteHeader(http.StatusBadGateway)
		w.Write(line) //nolint:errcheck // surface whatever brokerd said (e.g. an early error event)
		return
	}
	// Got the id. Close the brokerd connection — the task is detached from it.
	resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": ev.TaskID})
}
