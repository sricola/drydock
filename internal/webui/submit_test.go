package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postSubmit(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/submit", strings.NewReader(body))
	req.Host = "127.0.0.1:7878"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSubmitRejectsAutoApprove(t *testing.T) {
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("brokerd must not be called when auto_approve is set")
	}))}
	rec := postSubmit(t, s, `{"repo_ref":"https://x.test/r.git","instruction":"y","auto_approve":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("auto_approve submit = %d, want 400", rec.Code)
	}
}

func TestSubmitHappyPathReturnsID(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		io.WriteString(w, `{"event":"accepted","task_id":"`+id+`","repo":"https://x.test/r.git"}`+"\n")
		w.(http.Flusher).Flush()
		// then block as a real task would; the UI server should have closed already
		<-r.Context().Done()
	}))}
	rec := postSubmit(t, s, `{"repo_ref":"https://x.test/r.git","instruction":"y"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d, want 200", rec.Code)
	}
	var got struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ID != id {
		t.Fatalf("returned id = %q, want %q", got.ID, id)
	}
}

func TestSubmitSurfacesPreAcceptError(t *testing.T) {
	s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "repo_ref must be an https/git/ssh URL", http.StatusBadRequest)
	}))}
	rec := postSubmit(t, s, `{"repo_ref":"/local/path","instruction":"y"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "https/git/ssh") {
		t.Fatalf("error body not surfaced: %q", rec.Body.String())
	}
}

// A body that is a JSON object followed by trailing bytes is not "not JSON":
// brokerd's streaming decoder reads the first value and ignores the rest, so
// the auto_approve probe must never be skipped just because the whole body
// failed to parse. The UI must refuse anything that is not exactly one JSON
// object before it forwards a byte.
func TestSubmit_TrailingBytes(t *testing.T) {
	for _, body := range []string{
		`{"repo_ref":"https://x.test/r.git","instruction":"y","auto_approve":true} x`,
		`{"repo_ref":"https://x.test/r.git","instruction":"y","auto_approve":true}{}`,
		`{"repo_ref":"https://x.test/r.git","instruction":"y","auto_approve":true}` + "\n" + `[1]`,
		// No auto_approve at all: the trailing bytes alone must be refused,
		// otherwise this test would pass on the auto_approve refusal alone.
		`{"repo_ref":"https://x.test/r.git","instruction":"y"} x`,
	} {
		s := &Server{Token: "secret", BrokerDial: fakeBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("brokerd must not be called for body %q", body)
		}))}
		rec := postSubmit(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: submit = %d, want 400", body, rec.Code)
		}
		if !strings.Contains(body, "auto_approve") && !strings.Contains(rec.Body.String(), "single JSON object") {
			t.Errorf("body %q: error %q should say the body must be a single JSON object", body, rec.Body.String())
		}
	}
}
