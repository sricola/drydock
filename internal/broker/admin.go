package broker

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"drydock/internal/config"
)

// HandleApprove signals the pending task's channel with approval, after
// validating that the request body acknowledges every second-look category the
// task requires (see signal). Wire as POST /admin/approve/{id}.
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
	var awaitingEgress, settingUp, running, verifying, pendingApproval, pushing int
	for _, t := range b.tasks {
		switch t.Stage {
		case StageAwaitingEgress:
			awaitingEgress++
		case StageSettingUp:
			settingUp++
		case StageRunning:
			running++
		case StageVerifying:
			verifying++
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
		"setting_up":       settingUp,
		"running":          running,
		"verifying":        verifying,
		"pending_approval": pendingApproval,
		"pushing":          pushing,
	})
}

// HandlePolicy returns the daemon's effective policy exactly as resolved at
// boot by config.Explain: the per-field provenance table plus the divergence
// hash. It is read-only and does no recomputation — brokerd stashes
// PolicyFields/PolicyHash once at startup, and this handler just reports them,
// so `drydock` (or an operator) can diff the running daemon's live policy
// against the on-disk config.yaml to catch file-vs-live drift after an edit
// that hasn't been picked up by a restart.
func (b *Broker) HandlePolicy(w http.ResponseWriter, r *http.Request) {
	fields := b.PolicyFields
	if fields == nil {
		fields = make([]config.Field, 0)
	}
	writeJSON(w, map[string]any{
		"fields": fields,
		"hash":   b.PolicyHash,
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

// maxAckBodyBytes caps the optional approve/deny request body. Acknowledgment
// lists are a handful of short category strings; anything bigger is abuse.
const maxAckBodyBytes = 4 << 10

// readAcks parses the optional approve body {"acknowledge":["ci-workflow",..]}.
// An empty body is fine (nil acks). A non-nil error means the body could not
// be read or parsed — the caller decides whether that fails closed (it does,
// whenever the task requires any acks).
func readAcks(w http.ResponseWriter, r *http.Request) ([]string, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAckBodyBytes))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var body struct {
		Acknowledge []string `json:"acknowledge"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	return body.Acknowledge, nil
}

// missingAcks returns the required categories NOT covered by acks. Nil when
// nothing is required. A parse failure (parseErr != nil) of a request for a
// task WITH required acks fails closed: everything counts as missing.
func missingAcks(required, acks []string, parseErr error) []string {
	if len(required) == 0 {
		return nil
	}
	if parseErr != nil {
		return required
	}
	var missing []string
	for _, req := range required {
		if !slices.Contains(acks, req) {
			missing = append(missing, req)
		}
	}
	return missing
}

// signal resolves a pending gate: approve (ok=true) or deny (ok=false).
//
// SECOND-LOOK ENFORCEMENT (fail-safe by construction): when the task entered
// the gate with required acknowledgment categories (b.requiredAcks, registered
// atomically with b.pending by awaitGate), an approve must acknowledge a
// SUPERSET of them via the request body {"acknowledge":[...]}. Validation
// happens HERE, strictly BEFORE anything is sent on the approval channel: an
// insufficient, empty, or unparseable approve returns 422 naming the missing
// categories and never touches the channel — the gate stays registered, the
// task stays pending, and a corrected approve can still succeed. There is no
// code path on which an approve with missing acks signals the gate, so there
// is no path on which it pushes. Deny ignores acks entirely (denying never
// needs acknowledgment), as does the egress gate (which registers no
// requiredAcks entry).
func (b *Broker) signal(w http.ResponseWriter, r *http.Request, ok bool) {
	id := r.PathValue("id")
	b.pendingMu.Lock()
	ch, exists := b.pending[id]
	required := b.requiredAcks[id]
	b.pendingMu.Unlock()
	if !exists {
		http.Error(w, "no such pending task", http.StatusNotFound)
		return
	}
	var acks []string
	if ok {
		var parseErr error
		acks, parseErr = readAcks(w, r)
		if missing := missingAcks(required, acks, parseErr); len(missing) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    "approval refused: second-look categories not acknowledged; task stays pending",
				"missing":  missing,
				"required": required,
			})
			return
		}
	}
	select {
	case ch <- gateReply{ok: ok, acks: acks}:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "already signaled", http.StatusConflict)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
