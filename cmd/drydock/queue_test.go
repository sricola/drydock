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
	for _, col := range []string{"ID", "STATE", "AGE", "ATTEMPTS", "CI", "REPO"} {
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

// TestQueueList_CIColumn renders the CI observation column. The load-bearing
// case is the FIRST one: an item with no observation shows "-", never anything
// that could be read as a pass. The rest pin the states the broker can
// actually record, all of which are broker-authored vocabulary — no CI log
// text exists in the payload to render (D3).
func TestQueueList_CIColumn(t *testing.T) {
	enq := time.Now().Add(-time.Minute).UnixMilli()
	cases := []struct {
		name string
		item queueListItem
		want string
	}{
		{"never watched shows a dash, never a pass", queueListItem{State: "completed"}, "-"},
		{"armed but nothing observed yet", queueListItem{State: "awaiting_ci", CIState: "pending", PRNumber: 42}, "pending #42"},
		{"observed pass", queueListItem{State: "completed", CIState: "passed", PRNumber: 42}, "passed #42"},
		{"observed failure", queueListItem{State: "ci_failed", CIState: "failed", PRNumber: 7}, "failed #7"},
		{"no checks configured is not a pass", queueListItem{State: "completed", CIState: "no_checks", PRNumber: 7}, "no_checks #7"},
		{"timed out", queueListItem{State: "dead_letter", CIState: "timed_out", PRNumber: 7}, "timed_out #7"},
		{"gave up", queueListItem{State: "dead_letter", CIState: "unknown", PRNumber: 7}, "unknown #7"},
		{"pr with no state yet", queueListItem{State: "awaiting_ci", PRNumber: 9}, "#9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := queueCICell(c.item); got != c.want {
				t.Errorf("queueCICell = %q, want %q", got, c.want)
			}
			c.item.ID = "0123456789abcdef0123456789abcdef"
			c.item.Repo = "https://github.com/o/r.git"
			c.item.EnqueuedAtMs = enq
			var buf bytes.Buffer
			renderQueueTable(&buf, []queueListItem{c.item})
			if !strings.Contains(buf.String(), c.want) {
				t.Errorf("table missing %q:\n%s", c.want, buf.String())
			}
			if !strings.Contains(buf.String(), c.item.State) {
				t.Errorf("table missing state %q:\n%s", c.item.State, buf.String())
			}
		})
	}
}

// TestQueueList_CICellIsSanitized: the CI state crosses into an operator's
// terminal, so it goes through safeCell like every other broker-sourced
// string. A tampered queue file must not put escape sequences on screen.
func TestQueueList_CICellIsSanitized(t *testing.T) {
	got := queueCICell(queueListItem{CIState: "failed\x1b[31m\x07\r\ninjected", PRNumber: 3})
	if strings.ContainsAny(got, "\x1b\x07\r\n") {
		t.Errorf("queueCICell leaked control bytes: %q", got)
	}
	var buf bytes.Buffer
	renderQueueTable(&buf, []queueListItem{{
		ID: "0123456789abcdef0123456789abcdef", Repo: "r",
		State: "ci_failed\x1b[0m", CIState: "failed\x1b[31m", PRNumber: 3,
		EnqueuedAtMs: time.Now().UnixMilli(),
	}})
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("rendered table leaked an escape:\n%q", buf.String())
	}
}

// TestQueueList_DecodesCIFields: the JSON contract with brokerd's
// queueItemView. Absent keys (every item written before the CI arc, and every
// unwatched item) decode to the zero value, which renders as "-".
func TestQueueList_DecodesCIFields(t *testing.T) {
	fakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"0123456789abcdef0123456789abcdef","repo":"r","state":"ci_failed","enqueued_at_ms":1,"attempts":1,"pr_number":42,"ci_state":"failed"},
		                {"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":"r","state":"completed","enqueued_at_ms":1,"attempts":1}]`)
	})
	items, err := fetchQueue()
	if err != nil {
		t.Fatalf("fetchQueue: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].PRNumber != 42 || items[0].CIState != "failed" {
		t.Errorf("watched item = %+v", items[0])
	}
	if items[1].PRNumber != 0 || items[1].CIState != "" {
		t.Errorf("unwatched item = %+v, want zero CI fields", items[1])
	}
	if got := queueCICell(items[1]); got != "-" {
		t.Errorf("unwatched CI cell = %q, want -", got)
	}
}

// TestQueueList_RetryColumn renders the bounded-CI-retry chain. The
// load-bearing case is the first one: an item outside a chain — which is every
// item on a stock install, where ci.max_attempts is 0 — shows "-", exactly as
// it did before the column existed.
func TestQueueList_RetryColumn(t *testing.T) {
	const parent = "1111111111111111111111111111aaaa"
	const child = "2222222222222222222222222222bbbb"
	cases := []struct {
		name string
		item queueListItem
		want string
	}{
		{"not in a chain", queueListItem{State: "completed"}, "-"},
		{"a chain root that spawned a retry",
			queueListItem{State: "ci_failed", RetryTaskID: child}, "#0 ->222222222222…"},
		{"a retry with no child of its own",
			queueListItem{State: "queued", RetryOf: parent, Attempt: 1}, "#1 <-111111111111…"},
		{"a middle link, both directions",
			queueListItem{State: "ci_failed", RetryOf: parent, RetryTaskID: child, Attempt: 2},
			"#2 <-111111111111… ->222222222222…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := queueRetryCell(c.item); got != c.want {
				t.Errorf("queueRetryCell = %q, want %q", got, c.want)
			}
			c.item.ID = "0123456789abcdef0123456789abcdef"
			c.item.Repo = "https://github.com/o/r.git"
			c.item.EnqueuedAtMs = time.Now().UnixMilli()
			var buf bytes.Buffer
			renderQueueTable(&buf, []queueListItem{c.item})
			if !strings.Contains(buf.String(), "RETRY") {
				t.Errorf("table missing the RETRY header:\n%s", buf.String())
			}
			if !strings.Contains(buf.String(), c.want) {
				t.Errorf("table missing %q:\n%s", c.want, buf.String())
			}
		})
	}
}

// TestQueueList_RetryCellIsSanitized: the chain ids cross into an operator's
// terminal like every other cell. They are broker-minted 32-hex values, but a
// tampered queue file is exactly the case safeCell exists for.
func TestQueueList_RetryCellIsSanitized(t *testing.T) {
	got := queueRetryCell(queueListItem{
		RetryOf:     "\x1b[31mred\x07\r\ninjected",
		RetryTaskID: "\x1b[0mmore",
		Attempt:     1,
	})
	if strings.ContainsAny(got, "\x1b\x07\r\n") {
		t.Errorf("queueRetryCell leaked control bytes: %q", got)
	}
}

// TestQueueList_DecodesRetryFields pins the JSON contract with brokerd's
// queueItemView for the chain fields. Absent keys — every item outside a
// chain, and every item written before B2 — decode to the zero value and
// render as "-".
func TestQueueList_DecodesRetryFields(t *testing.T) {
	fakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"0123456789abcdef0123456789abcdef","repo":"r","state":"ci_failed","enqueued_at_ms":1,"retry_task_id":"2222222222222222222222222222bbbb"},
		                {"id":"2222222222222222222222222222bbbb","repo":"r","state":"queued","enqueued_at_ms":1,"retry_of":"0123456789abcdef0123456789abcdef","attempt":1},
		                {"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":"r","state":"completed","enqueued_at_ms":1}]`)
	})
	items, err := fetchQueue()
	if err != nil {
		t.Fatalf("fetchQueue: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].RetryTaskID != items[1].ID || items[1].RetryOf != items[0].ID {
		t.Errorf("the chain did not decode in both directions: %+v", items)
	}
	if items[1].Attempt != 1 {
		t.Errorf("attempt = %d, want 1", items[1].Attempt)
	}
	if items[2].RetryOf != "" || items[2].RetryTaskID != "" || items[2].Attempt != 0 {
		t.Errorf("an item outside a chain decoded non-zero: %+v", items[2])
	}
	if got := queueRetryCell(items[2]); got != "-" {
		t.Errorf("chainless retry cell = %q, want -", got)
	}
}
