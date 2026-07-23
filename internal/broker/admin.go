package broker

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
)

// HandleApprove signals the pending task's channel with true. Wire as
// POST /admin/approve/{id}.
func (b *Broker) HandleApprove(w http.ResponseWriter, r *http.Request) { b.signal(w, r, true) }

// HandleDeny signals false. Wire as POST /admin/deny/{id}.
func (b *Broker) HandleDeny(w http.ResponseWriter, r *http.Request) { b.signal(w, r, false) }

// HandlePending returns the set of task IDs currently awaiting approval.
// Kept as IDs-only for the existing approve/deny CLI path; richer output
// lives at /admin/tasks.
func (b *Broker) HandlePending(w http.ResponseWriter, r *http.Request) {
	b.pendingMu.Lock()
	ids := make([]string, 0, len(b.pending))
	for k := range b.pending {
		ids = append(ids, k)
	}
	b.pendingMu.Unlock()
	writeJSON(w, ids)
}

// HandleTasks returns rich state for every task currently in flight
// (running, awaiting approval, or pushing). The result is sorted oldest-
// first so the CLI table is deterministic.
func (b *Broker) HandleTasks(w http.ResponseWriter, r *http.Request) {
	b.pendingMu.Lock()
	out := make([]*TaskState, 0, len(b.tasks))
	for _, t := range b.tasks {
		// Copy so the caller can't mutate the live state and we don't hold
		// the lock during JSON encoding.
		cp := *t
		out = append(out, &cp)
	}
	b.pendingMu.Unlock()
	// Stable order: oldest first (SortStableFunc keeps registration order for
	// tasks sharing a StartedAt at nanosecond precision).
	slices.SortStableFunc(out, func(a, b *TaskState) int {
		return cmp.Compare(a.StartedAt.UnixNano(), b.StartedAt.UnixNano())
	})
	writeJSON(w, out)
}

// HandleHealth is a liveness/readiness probe. Returns ok plus a coarse
// breakdown so launchd KeepAlive, `drydock status`, and `drydock init`'s
// eventual smoke probe can all use the same endpoint.
func (b *Broker) HandleHealth(w http.ResponseWriter, r *http.Request) {
	b.pendingMu.Lock()
	pending := len(b.pending)
	var awaitingEgress, running, pendingApproval, pushing int
	for _, t := range b.tasks {
		switch t.Stage {
		case StageAwaitingEgress:
			awaitingEgress++
		case StageRunning:
			running++
		case StagePending:
			pendingApproval++
		case StagePushing:
			pushing++
		}
	}
	b.pendingMu.Unlock()
	writeJSON(w, map[string]any{
		"ok":               true,
		"pending":          pending, // legacy field; matches old shape
		"awaiting_egress":  awaitingEgress,
		"running":          running,
		"pending_approval": pendingApproval,
		"pushing":          pushing,
	})
}

// HandleKill cancels the per-task context, which aborts the container run
// (if still in flight) and the push-gate wait (if at the approval gate).
// Returns 204 on success, 404 if no such live task. The corresponding
// `POST /tasks` request will return a body with "cancelled": true.
func (b *Broker) HandleKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b.pendingMu.Lock()
	cancel, ok := b.cancellers[id]
	b.pendingMu.Unlock()
	if !ok {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	cancel(errTaskKilled)
	w.WriteHeader(http.StatusNoContent)
}

func (b *Broker) signal(w http.ResponseWriter, r *http.Request, ok bool) {
	id := r.PathValue("id")
	b.pendingMu.Lock()
	ch, exists := b.pending[id]
	b.pendingMu.Unlock()
	if !exists {
		http.Error(w, "no such pending task", http.StatusNotFound)
		return
	}
	select {
	case ch <- ok:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "already signaled", http.StatusConflict)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
