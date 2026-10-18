package broker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The task endpoints must accept exactly one JSON object. A streaming decoder
// that stops at the first value lets a client (or a proxy in front of brokerd,
// like the web UI) see a different document from the one brokerd acts on:
// `{"auto_approve":true} x` fails a whole-body Unmarshal upstream yet decodes
// here with auto_approve set.
func TestHandleQueueAdd_RejectsTrailingBytesAfterTheJSONObject(t *testing.T) {
	b := queueBroker(t, 1, nil)
	b.StopDispatcher() // nothing should run; the item must not even be persisted
	for _, body := range []string{
		`{"repo_ref":"https://github.com/o/r.git","instruction":"y"} x`,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"y"}{}`,
		`{"repo_ref":"https://github.com/o/r.git","instruction":"y"}` + "\n" + `[1]`,
	} {
		req := httptest.NewRequest("POST", "/queue", strings.NewReader(body))
		rec := httptest.NewRecorder()
		b.HandleQueueAdd(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400 (body %q)", body, rec.Code, rec.Body.String())
		}
	}
	if items, _ := listQueueItems(b.AuditRoot); len(items) != 0 {
		t.Errorf("a rejected body persisted %d queue item(s)", len(items))
	}
}

func TestHandleTask_RejectsTrailingBytesAfterTheJSONObject(t *testing.T) {
	b := testBroker(t, "anthropic", &fakeStage{}, &fakeGrant{}, nil)
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(`{"repo_ref":"https://github.com/o/r.git","instruction":"y"} x`))
	rec := httptest.NewRecorder()
	b.HandleTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "single JSON object") {
		t.Errorf("error text %q should say the body must be a single JSON object", rec.Body.String())
	}
}
