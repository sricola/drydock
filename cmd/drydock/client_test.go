package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSocketPath_HonorsConfig verifies that socketPath reads broker.socket from
// config.yaml when BROKER_SOCKET is not set, matching the resolution order in
// brokerd (cfg.Broker.Socket before sockpath.Default()).
func TestSocketPath_HonorsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BROKER_SOCKET", "") // ensure env override is off

	cfgDir := filepath.Join(home, ".drydock")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"),
		[]byte("broker:\n  socket: /tmp/custom-test.sock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := socketPath()
	if got != "/tmp/custom-test.sock" {
		t.Errorf("socketPath() = %q, want /tmp/custom-test.sock (from config broker.socket)", got)
	}
}

// TestBrokerdDown_ConfigOnlyTCP verifies that a TCP broker configured only via
// config.yaml (broker.addr, no BROKER_ADDR env) is treated as TCP mode — so a
// dial failure does NOT get the misleading "brokerd not running, run drydock
// start" socket hint (which only applies to the unix-socket transport).
func TestBrokerdDown_ConfigOnlyTCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BROKER_ADDR", "") // config is the only source of the TCP addr

	cfgDir := filepath.Join(home, ".drydock")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"),
		[]byte("broker:\n  addr: 127.0.0.1:19999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if brokerdDown(errors.New("dial tcp 127.0.0.1:19999: connect: connection refused")) {
		t.Error("brokerdDown must be false in config-only TCP mode (no socket hint)")
	}
}

// TestPrintClientErr_BrokerDown_PrintsFriendlyHint verifies that when the
// broker socket is absent (brokerdDown=true), printClientErr substitutes the
// human-readable hint instead of the raw Go dial error. This is the primary
// UX gate: a first-time operator should see "start brokerd", not an opaque
// transport error.
func TestPrintClientErr_BrokerDown_PrintsFriendlyHint(t *testing.T) {
	// Point BROKER_SOCKET at a file that does not exist — os.Stat returns
	// IsNotExist, which is the signal brokerdDown uses.
	missing := filepath.Join(t.TempDir(), "no-such.sock")
	t.Setenv("BROKER_SOCKET", missing)
	t.Setenv("BROKER_ADDR", "")
	// Isolate from any real ~/.drydock/config.yaml that sets broker.addr.
	t.Setenv("HOME", t.TempDir())

	var buf bytes.Buffer
	orig := errOut
	t.Cleanup(func() { errOut = orig })
	errOut = &buf

	printClientErr(errors.New("dial unix " + missing + ": no such file or directory"))

	got := buf.String()
	if !strings.Contains(got, brokerDownHint) {
		t.Errorf("expected brokerDownHint %q in output; got: %q", brokerDownHint, got)
	}
}

// TestPrintClientErr_OtherError_PrintsRawError verifies that when brokerd IS
// reachable (or in TCP mode), printClientErr does not substitute the socket
// hint — it prints the raw error so the operator sees the actual problem.
func TestPrintClientErr_OtherError_PrintsRawError(t *testing.T) {
	// TCP mode (BROKER_ADDR set) — brokerdDown returns false immediately.
	t.Setenv("BROKER_ADDR", "127.0.0.1:19999")

	var buf bytes.Buffer
	orig := errOut
	t.Cleanup(func() { errOut = orig })
	errOut = &buf

	myErr := errors.New("unexpected HTTP 503")
	printClientErr(myErr)

	got := buf.String()
	if strings.Contains(got, brokerDownHint) {
		t.Errorf("must NOT print brokerDownHint for non-down TCP error; got: %q", got)
	}
	if !strings.Contains(got, myErr.Error()) {
		t.Errorf("raw error %q missing from output; got: %q", myErr.Error(), got)
	}
}

// TestDoSignalApproveSendsAckBody pins the HTTP contract for an acknowledged
// approve: the acks marshal into a {"acknowledge":[...]} JSON body with
// Content-Type application/json, POSTed to /admin/approve/<id>.
func TestDoSignalApproveSendsAckBody(t *testing.T) {
	var gotBody []byte
	var gotCT string
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/approve/task-1" {
			t.Errorf("request = %s %s, want POST /admin/approve/task-1", r.Method, r.URL.Path)
		}
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	out := captureStdout(t, func() {
		if code := doSignal("approve", "task-1", []string{"ci-workflow", "lockfile"}); code != 0 {
			t.Errorf("doSignal = %d, want 0", code)
		}
	})
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var body struct {
		Acknowledge []string `json:"acknowledge"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("unmarshal ack body %q: %v", gotBody, err)
	}
	if len(body.Acknowledge) != 2 || body.Acknowledge[0] != "ci-workflow" || body.Acknowledge[1] != "lockfile" {
		t.Errorf("acknowledge = %v, want [ci-workflow lockfile]", body.Acknowledge)
	}
	if !strings.Contains(out, "task task-1 approved") {
		t.Errorf("output = %q, want approved confirmation", out)
	}
}

// TestDoSignalApproveWithoutAcksSendsEmptyBody preserves the pre-ack contract:
// a plain approve (and every deny) still POSTs an empty body.
func TestDoSignalApproveWithoutAcksSendsEmptyBody(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			var gotBody []byte
			var gotCT string
			useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCT = r.Header.Get("Content-Type")
				gotBody, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			captureStdout(t, func() {
				if code := doSignal(verb, "task-1", nil); code != 0 {
					t.Errorf("doSignal = %d, want 0", code)
				}
			})
			if len(gotBody) != 0 {
				t.Errorf("%s body = %q, want empty", verb, gotBody)
			}
			if gotCT == "application/json" {
				t.Errorf("%s Content-Type = %q, want no JSON body declared", verb, gotCT)
			}
		})
	}
}

// TestDoSignalDenyNeverSendsAcks: deny ignores acknowledgments entirely —
// even if a caller passes some, the body stays empty (denying never needs
// acknowledgment, and the broker ignores any body on deny).
func TestDoSignalDenyNeverSendsAcks(t *testing.T) {
	var gotBody []byte
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	captureStdout(t, func() { doSignal("deny", "task-1", []string{"ci-workflow"}) })
	if len(gotBody) != 0 {
		t.Errorf("deny body = %q, want empty even when acks passed", gotBody)
	}
}

// TestDoSignalApprove422PrintsMissingAndHint: brokerd's 422 refusal names the
// missing second-look categories; the CLI must surface them plus a re-run
// hint with --acknowledge per required category, and exit nonzero.
func TestDoSignalApprove422PrintsMissingAndHint(t *testing.T) {
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"approval refused: second-look categories not acknowledged; task stays pending","missing":["ci-workflow"],"required":["ci-workflow","lockfile"]}`)
	}))

	var buf bytes.Buffer
	orig := errOut
	t.Cleanup(func() { errOut = orig })
	errOut = &buf

	var code int
	captureStdout(t, func() { code = doSignal("approve", "task-1", []string{"lockfile"}) })
	if code != 1 {
		t.Errorf("doSignal = %d, want 1", code)
	}
	got := buf.String()
	for _, want := range []string{
		"ci-workflow", // the missing category, by name
		"--acknowledge",
		"drydock approve task-1 --acknowledge ci-workflow --acknowledge lockfile",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("422 output missing %q:\n%s", want, got)
		}
	}
}

// TestDoSignalApprove422UnparseableBodyStillRefuses: a 422 with a garbage
// body must still exit 1 with a generic second-look refusal (never a crash,
// never success).
func TestDoSignalApprove422UnparseableBodyStillRefuses(t *testing.T) {
	useBrokerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, "not json")
	}))
	var buf bytes.Buffer
	orig := errOut
	t.Cleanup(func() { errOut = orig })
	errOut = &buf

	var code int
	captureStdout(t, func() { code = doSignal("approve", "task-1", nil) })
	if code != 1 {
		t.Errorf("doSignal = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "second-look") {
		t.Errorf("output = %q, want a second-look refusal mention", buf.String())
	}
}

func TestPastTense(t *testing.T) {
	cases := map[string]string{"approve": "approved", "deny": "denied"}
	for verb, want := range cases {
		if got := pastTense(verb); got != want {
			t.Errorf("pastTense(%q) = %q, want %q", verb, got, want)
		}
	}
}
