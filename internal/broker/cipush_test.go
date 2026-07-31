package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/remote"
)

// capturingAdapter is a fakeAdapter that ALSO implements the optional
// remote.PullRequestOpener capability, so the tests below can drive the
// PR-identity path without shelling out to gh.
type capturingAdapter struct {
	fakeAdapter
	pr          remote.PullRequest
	resultErr   error
	resultCalls int
	// onResult runs inside OpenRequestResult, after the Request is recorded.
	// The marker-write-failure test uses it to sabotage the marker path using
	// the task id it can only learn from the branch name.
	onResult func(remote.Request)
}

func (a *capturingAdapter) OpenRequestResult(r remote.Request) (remote.PullRequest, error) {
	a.resultCalls++
	a.gotReq = r
	a.opened = true
	if a.onResult != nil {
		a.onResult(r)
	}
	return a.pr, a.resultErr
}

const pushBody = `{"repo_ref":"https://github.com/o/r.git","instruction":"do x","agent":"claude","auto_approve":true}`

func ciTestBroker(t *testing.T, st taskStage, adapter remote.Adapter) *Broker {
	t.Helper()
	resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"total_cost_usd":0.01,"num_turns":1}`
	b := testBroker(t, "anthropic", st, &fakeGrant{}, writesResult(resultLine))
	b.newAdapter = func(string, string) remote.Adapter { return adapter }
	return b
}

// markerFiles returns every *.ci.json in the audit dir.
func markerFiles(t *testing.T, dir string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(dir, "*.ci.json"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The happy path: watch on, adapter reports identity -> the terminal event
// carries pr_number/pr_url and a durable marker lands in the audit dir.
func TestFinishPush_CIWatchOn_WritesMarker(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          remote.PullRequest{Number: 42, URL: "https://github.com/o/r/pull/42"},
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true
	b.now = func() int64 { return 1700000000000 }

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed", term["outcome"])
	}
	if ad.resultCalls != 1 {
		t.Errorf("OpenRequestResult calls = %d, want exactly 1 (never both paths — that would open two PRs)", ad.resultCalls)
	}
	if term["pr_opened"] != true {
		t.Errorf("pr_opened = %v, want true", term["pr_opened"])
	}
	if term["pr_number"] != float64(42) || term["pr_url"] != "https://github.com/o/r/pull/42" {
		t.Errorf("pr_number/pr_url = %v/%v, want 42 / the PR URL", term["pr_number"], term["pr_url"])
	}

	id, _ := term["task_id"].(string)
	files := markerFiles(t, b.AuditRoot)
	if len(files) != 1 {
		t.Fatalf("marker files = %v, want exactly one", files)
	}
	m, err := readCIMarker(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("readCIMarker: %v", err)
	}
	if m.TaskID != id || m.PRNumber != 42 || m.PRURL != "https://github.com/o/r/pull/42" {
		t.Errorf("marker identity = %+v", m)
	}
	if m.Branch != "agent/"+id {
		t.Errorf("marker branch = %q, want %q", m.Branch, "agent/"+id)
	}
	if m.RepoRef != "https://github.com/o/r.git" {
		t.Errorf("marker repo_ref = %q", m.RepoRef)
	}
	// Nothing has been observed yet: pending, never a default of "passed".
	if m.State != CIPending {
		t.Errorf("marker state = %q, want %q", m.State, CIPending)
	}
	if m.CreatedAtMs != 1700000000000 || m.UpdatedAtMs != 1700000000000 {
		t.Errorf("timestamps = %d/%d, want the broker clock seam's value", m.CreatedAtMs, m.UpdatedAtMs)
	}
	// B2's chain fields are present in the schema and zero for a first attempt.
	if m.Attempt != 0 || m.RetryOf != "" {
		t.Errorf("attempt/retry_of = %d/%q, want 0/\"\" for a first attempt", m.Attempt, m.RetryOf)
	}
}

// Watch OFF (the default) must be byte-identical to the pre-CI behavior: the
// plain OpenRequest path, no capability call, no marker, and no new event keys.
func TestFinishPush_CIWatchOff_IsTodaysPathExactly(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          remote.PullRequest{Number: 42, URL: "https://github.com/o/r/pull/42"},
	}
	b := ciTestBroker(t, st, ad) // CIWatch defaults to false

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" || term["pr_opened"] != true {
		t.Fatalf("outcome=%v pr_opened=%v, want pushed/true", term["outcome"], term["pr_opened"])
	}
	if ad.resultCalls != 0 {
		t.Errorf("OpenRequestResult was called %d times with the watch off; want 0", ad.resultCalls)
	}
	if !ad.opened {
		t.Error("the plain OpenRequest path must still have opened the PR")
	}
	if _, ok := term["pr_number"]; ok {
		t.Error("pr_number must be absent with the watch off")
	}
	if _, ok := term["pr_url"]; ok {
		t.Error("pr_url must be absent with the watch off")
	}
	if files := markerFiles(t, b.AuditRoot); len(files) != 0 {
		t.Errorf("markers written with the watch off: %v", files)
	}
}

// An adapter without the capability (gitlab/gitea/push-only, and every test
// fake) must keep working untouched even with the watch on.
func TestFinishPush_CapabilityLessAdapter_NoMarker(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &fakeAdapter{name: "gitlab"}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" || term["pr_opened"] != true {
		t.Fatalf("outcome=%v pr_opened=%v, want pushed/true", term["outcome"], term["pr_opened"])
	}
	if !ad.opened {
		t.Error("OpenRequest must still be called on a capability-less adapter")
	}
	if _, ok := term["pr_number"]; ok {
		t.Error("pr_number must be absent without the capability")
	}
	if files := markerFiles(t, b.AuditRoot); len(files) != 0 {
		t.Errorf("markers written for a capability-less adapter: %v", files)
	}
}

// gh succeeded but printed nothing parseable: missing evidence. No marker, no
// watch, and — critically — still a successful push.
func TestFinishPush_NoPRIdentity_StillPushed_NoMarker(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{fakeAdapter: fakeAdapter{name: "github"}} // zero PullRequest, nil error
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed", term["outcome"])
	}
	if term["pr_opened"] != true {
		t.Errorf("pr_opened = %v, want true (the open itself succeeded)", term["pr_opened"])
	}
	if _, ok := term["pr_number"]; ok {
		t.Error("pr_number must be absent when no identity was observed")
	}
	if files := markerFiles(t, b.AuditRoot); len(files) != 0 {
		t.Errorf("markers written without a PR identity: %v", files)
	}
	if !st.pushed.Load() {
		t.Error("the branch must still have been pushed")
	}
}

// A capture failure is a PR-open failure: pr_opened=false, no marker, and the
// push still reports success. A landed branch must never be downgraded.
func TestFinishPush_CaptureError_NeverFailsThePush(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		resultErr:   errors.New("gh: not authenticated"),
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed (a saved branch must never report failure)", term["outcome"])
	}
	if term["pr_opened"] != false {
		t.Errorf("pr_opened = %v, want false", term["pr_opened"])
	}
	if term["pr_error"] == nil {
		t.Error("pr_error should carry the capture failure reason")
	}
	if _, ok := term["pr_number"]; ok {
		t.Error("pr_number must be absent when the capture failed")
	}
	if files := markerFiles(t, b.AuditRoot); len(files) != 0 {
		t.Errorf("markers written after a failed capture: %v", files)
	}
	if !st.pushed.Load() {
		t.Error("the branch must still have been pushed")
	}
}

// A marker write that fails on disk must not touch the outcome either: the
// push already landed and the terminal event is already assembled. Sabotage is
// a directory planted at the marker path, which makes atomicfile's rename fail.
func TestFinishPush_MarkerWriteFailure_NeverFailsThePush(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          remote.PullRequest{Number: 7, URL: "https://github.com/o/r/pull/7"},
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true
	ad.onResult = func(r remote.Request) {
		id := strings.TrimPrefix(r.Branch, "agent/")
		if err := os.Mkdir(filepath.Join(b.AuditRoot, id+".ci.json"), 0o700); err != nil {
			t.Errorf("planting the sabotage dir: %v", err)
		}
	}

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed despite the marker write failing", term["outcome"])
	}
	if term["pr_opened"] != true || term["pr_number"] != float64(7) {
		t.Errorf("pr_opened=%v pr_number=%v, want true/7 (the observed identity still reports)",
			term["pr_opened"], term["pr_number"])
	}
	id, _ := term["task_id"].(string)
	if _, err := readCIMarker(b.AuditRoot, id); err == nil {
		t.Error("expected no readable marker after the sabotaged write")
	}
}

// collidingStage rejects the first push as a branch-name collision, so
// pushWithRecovery lands on agent/<id>-2.
type collidingStage struct {
	*fakeStage
	calls int
}

func (c *collidingStage) PushBranch(local, remoteBranch string) error {
	c.calls++
	if c.calls == 1 {
		return errNonFF
	}
	return c.fakeStage.PushBranch(local, remoteBranch)
}

// The marker (and the PR request) must key off the branch pushWithRecovery
// RETURNED, never the base name: on a collision the pushed branch is
// agent/<id>-2 and a marker naming agent/<id> would watch a branch this task
// never pushed.
func TestFinishPush_MarkerKeysOffReturnedBranch(t *testing.T) {
	st := &collidingStage{fakeStage: &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          remote.PullRequest{Number: 99, URL: "https://github.com/o/r/pull/99"},
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true
	b.PushFreshBranchTries = 2

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed", term["outcome"])
	}
	id, _ := term["task_id"].(string)
	want := "agent/" + id + "-2"
	if term["branch"] != want {
		t.Fatalf("branch = %v, want %q", term["branch"], want)
	}
	if ad.gotReq.Branch != want {
		t.Errorf("PR request branch = %q, want the pushed branch %q", ad.gotReq.Branch, want)
	}
	m, err := readCIMarker(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("readCIMarker: %v", err)
	}
	if m.Branch != want {
		t.Errorf("marker branch = %q, want the pushed branch %q", m.Branch, want)
	}
}
