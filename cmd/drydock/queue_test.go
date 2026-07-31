package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeBroker starts an httptest server and points BROKER_ADDR at it for the
// duration of the test, so the CLI's brokerclient resolves to it.
func fakeBroker(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("BROKER_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	return srv
}

// TestQueueAdd_PostsTaskBody: `drydock queue add` POSTs the taskRequest to
// /queue (not /tasks) and surfaces the returned task id.
func TestQueueAdd_PostsTaskBody(t *testing.T) {
	var gotPath, gotMethod string
	var gotReq taskRequest
	fakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode posted body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"event":"queued","task_id":"0123456789abcdef0123456789abcdef"}`)
	})

	want := taskRequest{
		RepoRef:     "https://github.com/o/r",
		Instruction: "do x",
		EgressExtra: []domain{{Host: "api.example.com", Ports: []int{443}}},
		AutoApprove: true,
		Agent:       "claude",
	}
	id, err := postQueueAdd(want)
	if err != nil {
		t.Fatalf("postQueueAdd: %v", err)
	}
	if id != "0123456789abcdef0123456789abcdef" {
		t.Errorf("id = %q", id)
	}
	if gotMethod != "POST" || gotPath != "/queue" {
		t.Errorf("request = %s %s, want POST /queue", gotMethod, gotPath)
	}
	if !reflect.DeepEqual(gotReq, want) {
		t.Errorf("posted body = %+v, want %+v", gotReq, want)
	}
}

func TestQueueAdd_BrokerErrorSurfaces(t *testing.T) {
	fakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "repo_ref must be an https/git/ssh URL (no local paths)", http.StatusBadRequest)
	})
	_, err := postQueueAdd(taskRequest{RepoRef: "/nope", Instruction: "x"})
	if err == nil || !strings.Contains(err.Error(), "repo_ref") {
		t.Fatalf("err = %v, want the broker's 400 text surfaced", err)
	}
}

// TestQueueList_FetchAndRender: `drydock queue list` GETs /queue and renders
// ID/STATE/AGE/ATTEMPTS/REPO, with AGE derived from enqueued_at_ms.
func TestQueueList_FetchAndRender(t *testing.T) {
	enq := time.Now().Add(-3 * time.Minute).UnixMilli()
	var gotPath, gotMethod string
	fakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":"0123456789abcdef0123456789abcdef","repo":"https://github.com/o/r.git","state":"queued","enqueued_at_ms":%d,"attempts":2}]`, enq)
	})

	items, err := fetchQueue()
	if err != nil {
		t.Fatalf("fetchQueue: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/queue" {
		t.Errorf("request = %s %s, want GET /queue", gotMethod, gotPath)
	}
	if len(items) != 1 || items[0].State != "queued" || items[0].Attempts != 2 {
		t.Fatalf("items = %+v", items)
	}

	var buf bytes.Buffer
	renderQueueTable(&buf, items)
	out := buf.String()
	for _, col := range []string{"ID", "STATE", "AGE", "ATTEMPTS", "REPO"} {
		if !strings.Contains(out, col) {
			t.Errorf("table missing %s column:\n%s", col, out)
		}
	}
	for _, cell := range []string{"0123456789abcdef0123456789abcdef", "queued", "3m", "2", "o/r.git"} {
		if !strings.Contains(out, cell) {
			t.Errorf("table missing cell %q:\n%s", cell, out)
		}
	}
}

func TestQueueList_EmptyQueue(t *testing.T) {
	var buf bytes.Buffer
	renderQueueTable(&buf, nil)
	if !strings.Contains(buf.String(), "queue is empty") {
		t.Errorf("empty render = %q, want a friendly empty message", buf.String())
	}
}

// TestQueueCancel_PostsRightPath: cancel POSTs /queue/cancel/{id}; 204 is
// success (exit 0), 404 is unknown id (exit 1).
func TestQueueCancel_PostsRightPath(t *testing.T) {
	var gotPath, gotMethod string
	fakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	if code := postQueueCancel("0123456789abcdef0123456789abcdef"); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if gotMethod != "POST" || gotPath != "/queue/cancel/0123456789abcdef0123456789abcdef" {
		t.Errorf("request = %s %s, want POST /queue/cancel/<id>", gotMethod, gotPath)
	}
}

func TestQueueCancel_404IsFailure(t *testing.T) {
	fakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such task", http.StatusNotFound)
	})
	var errBuf bytes.Buffer
	orig := errOut
	errOut = &errBuf
	defer func() { errOut = orig }()
	if code := postQueueCancel("deadbeefdeadbeefdeadbeefdeadbeef"); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no such") {
		t.Errorf("stderr = %q, want a no-such-task diagnostic", errBuf.String())
	}
}

// TestSharedBuilder_SubmitAndQueueAddIdentical proves the two commands share
// one flag->taskRequest builder: the same argv yields byte-identical requests,
// so the flag surface can never drift between `submit` and `queue add`.
func TestSharedBuilder_SubmitAndQueueAddIdentical(t *testing.T) {
	args := []string{
		"--repo", "https://github.com/o/r",
		"--instruction", "do x",
		"--egress-extra", "api.example.com:443,8443",
		"--egress-extra", "b.example.com:80",
		"--auto-approve", "--sensitive", "--draft",
		"--platform", "github", "--model", "some-model", "--agent", "claude",
	}
	sub, _, _ := parseSubmitRequest(args)
	qa, _, _ := parseQueueAddRequest(args)
	if !reflect.DeepEqual(sub, qa) {
		t.Errorf("builders diverge:\nsubmit    = %+v\nqueue add = %+v", sub, qa)
	}
	if sub.RepoRef != "https://github.com/o/r" || !sub.AutoApprove || len(sub.EgressExtra) != 2 {
		t.Errorf("builder output wrong: %+v", sub)
	}
}
