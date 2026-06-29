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
