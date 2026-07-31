package broker

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests drive the queue's HTTP surface (Task 4): POST /queue ->
// HandleQueueAdd, GET /queue -> HandleQueueList (a projected view, never the
// full Task), POST /queue/cancel/{id} -> HandleQueueCancel (dequeue when still
// queued; delegate to the live-task kill path otherwise). They reuse the
// queueBroker harness from queue_test.go and deliberately never start the
// dispatcher — the handlers' contract is durable state, not execution.

func TestHandleQueueAdd_EnqueuesAndReturnsID(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	body := `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","auto_approve":true,"agent":"claude"}`
	req := httptest.NewRequest("POST", "/queue", strings.NewReader(body))
	rr := httptest.NewRecorder()
	b.HandleQueueAdd(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var resp struct {
		Event  string `json:"event"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body)
	}
	if resp.Event != "queued" {
		t.Errorf("event = %q, want queued", resp.Event)
	}
	if !queueIDRE.MatchString(resp.TaskID) {
		t.Errorf("task_id = %q, want 32-hex", resp.TaskID)
	}
	items, err := listQueueItems(b.AuditRoot)
	if err != nil || len(items) != 1 {
		t.Fatalf("listQueueItems: n=%d err=%v, want 1 durably queued item", len(items), err)
	}
	it := items[0]
	if it.ID != resp.TaskID || it.State != QueueQueued {
		t.Errorf("item id=%q state=%q, want %q/queued", it.ID, it.State, resp.TaskID)
	}
	if !it.Task.AutoApprove || it.Task.Instruction != "do x" || it.Task.Agent != "claude" {
		t.Errorf("Task not preserved: %+v", it.Task)
	}
	if it.EnqueuedAtMs == 0 {
		t.Error("EnqueuedAtMs not stamped")
	}
}

func TestHandleQueueAdd_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"garbage json", `{not json`},
		{"local path repo", `{"repo_ref":"/local/path","instruction":"x"}`},
		{"wildcard egress", `{"repo_ref":"https://github.com/o/r.git","instruction":"x","egress_extra":[{"host":"*","ports":[443]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
			req := httptest.NewRequest("POST", "/queue", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			b.HandleQueueAdd(rr, req)
			if rr.Code != 400 {
				t.Fatalf("code=%d body=%s, want 400", rr.Code, rr.Body)
			}
			if items, _ := listQueueItems(b.AuditRoot); len(items) != 0 {
				t.Errorf("rejected add persisted %d items, want 0", len(items))
			}
		})
	}
}

// TestHandleQueueAdd_CapsBody pins the same MaxBytesReader cap POST /tasks
// enforces: an oversized body is rejected, nothing enqueued.
func TestHandleQueueAdd_CapsBody(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	body := `{"repo_ref":"https://github.com/o/r.git","instruction":"` +
		strings.Repeat("a", MaxTaskBodyBytes) + `"}`
	req := httptest.NewRequest("POST", "/queue", strings.NewReader(body))
	rr := httptest.NewRecorder()
	b.HandleQueueAdd(rr, req)
	if rr.Code != 400 {
		t.Fatalf("code=%d, want 400 for an over-cap body", rr.Code)
	}
	if items, _ := listQueueItems(b.AuditRoot); len(items) != 0 {
		t.Errorf("over-cap add persisted %d items, want 0", len(items))
	}
}

func TestHandleQueueList_ProjectedView(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	// Deterministic clock: two wall-clock Enqueues can land in the same
	// millisecond, which would leave the FIFO tie-break to directory order.
	var clock int64
	b.now = func() int64 { clock += 1000; return clock }
	id1, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "secret prompt one"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id2, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r2.git", Instruction: "secret prompt two"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	rr := httptest.NewRecorder()
	b.HandleQueueList(rr, httptest.NewRequest("GET", "/queue", nil))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body)
	}
	if len(got) != 2 {
		t.Fatalf("n=%d items, want 2", len(got))
	}
	// FIFO order (sorted by EnqueuedAtMs).
	if got[0]["id"] != id1 || got[1]["id"] != id2 {
		t.Errorf("order = [%v %v], want [%s %s]", got[0]["id"], got[1]["id"], id1, id2)
	}
	first := got[0]
	if first["repo"] != "https://github.com/o/r.git" || first["state"] != "queued" {
		t.Errorf("projection fields wrong: %v", first)
	}
	if ms, ok := first["enqueued_at_ms"].(float64); !ok || ms <= 0 {
		t.Errorf("enqueued_at_ms = %v, want a positive number", first["enqueued_at_ms"])
	}
	if _, ok := first["attempts"]; !ok {
		t.Errorf("attempts missing from projection: %v", first)
	}
	// The projection must NOT leak the full Task (instruction text etc.).
	if _, ok := first["task"]; ok {
		t.Error("projected view leaked the full task object")
	}
	if strings.Contains(rr.Body.String(), "secret prompt") {
		t.Error("projected view leaked the instruction text")
	}
}

func TestHandleQueueList_EmptyIsJSONArray(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	rr := httptest.NewRecorder()
	b.HandleQueueList(rr, httptest.NewRequest("GET", "/queue", nil))
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("empty queue: code=%d body=%q, want 200 []", rr.Code, rr.Body)
	}
}

func TestHandleQueueCancel_DequeuesQueuedItem(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	id, err := b.Enqueue(Task{RepoRef: "https://github.com/o/r.git", Instruction: "x"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	req := httptest.NewRequest("POST", "/queue/cancel/"+id, nil)
	req.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	b.HandleQueueCancel(rr, req)
	if rr.Code != 204 {
		t.Fatalf("code=%d, want 204", rr.Code)
	}
	if it := queueItemState(t, b, id); it.State != QueueCancelled {
		t.Errorf("state = %q, want cancelled", it.State)
	}
}

func TestHandleQueueCancel_404Unknown(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	req := httptest.NewRequest("POST", "/queue/cancel/deadbeefdeadbeefdeadbeefdeadbeef", nil)
	req.SetPathValue("id", "deadbeefdeadbeefdeadbeefdeadbeef")
	rr := httptest.NewRecorder()
	b.HandleQueueCancel(rr, req)
	if rr.Code != 404 {
		t.Fatalf("code=%d, want 404", rr.Code)
	}
}

// TestHandleQueueCancel_DelegatesToKillWhenRunning: an item that already left
// the queue but has a live canceller (i.e. it is running) is killed via the
// SAME path HandleKill uses — the stored per-task context cancel.
func TestHandleQueueCancel_DelegatesToKillWhenRunning(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	const id = "0123456789abcdef0123456789abcdef"
	ctx, cancel := context.WithCancelCause(context.Background())
	b.registerTask(id, "https://github.com/o/r.git", "x", cancel)

	req := httptest.NewRequest("POST", "/queue/cancel/"+id, nil)
	req.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	b.HandleQueueCancel(rr, req)
	if rr.Code != 204 {
		t.Fatalf("code=%d, want 204 (kill delegation)", rr.Code)
	}
	select {
	case <-ctx.Done():
		if cause := context.Cause(ctx); cause != errTaskKilled {
			t.Errorf("cancel cause = %v, want errTaskKilled", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("stored cancel never fired")
	}
}

// TestHandleQueueList_ProjectsCIFields: the CI observation reaches the CLI
// through the projected view. Both fields are omitempty, so an unwatched item
// serialises to exactly its pre-CI shape — the absence of ci_state is the
// wire-level statement "nothing was observed", and it must never be filled in
// with a default.
func TestHandleQueueList_ProjectsCIFields(t *testing.T) {
	b := queueBroker(t, 2, writesResult(`{"type":"result","subtype":"success"}`))
	watched := seedQueuedItem(t, b, QueueAwaitingCI)
	unwatched := seedQueuedItem(t, b, QueueAwaitingReview)

	rr := httptest.NewRecorder()
	b.HandleQueueList(rr, httptest.NewRequest("GET", "/queue", nil))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, it := range got {
		byID[it["id"].(string)] = it
	}
	w := byID[watched.ID]
	if w["state"] != "awaiting_ci" || w["ci_state"] != "pending" || w["pr_number"] != float64(42) {
		t.Errorf("watched projection = %v", w)
	}
	u := byID[unwatched.ID]
	if _, present := u["ci_state"]; present {
		t.Errorf("unwatched item carries ci_state = %v, want the key omitted entirely", u["ci_state"])
	}
	if _, present := u["pr_number"]; present {
		t.Errorf("unwatched item carries pr_number = %v, want the key omitted", u["pr_number"])
	}
}
