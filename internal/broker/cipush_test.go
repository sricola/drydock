package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	// NOTE: openCalls is deliberately NOT touched here. It counts OpenRequest
	// only, so a test on the capability path can assert openCalls == 0 and
	// actually witness a double open (two PRs) if one ever happens.
	if a.onResult != nil {
		a.onResult(r)
	}
	return a.pr, a.resultErr
}

// prIdentity is the shape a real capture produces: a validated owner/repo
// alongside the number and URL.
func prIdentity(n int, owner, repo string) remote.PullRequest {
	return remote.PullRequest{Number: n, Owner: owner, Repo: repo,
		URL: "https://github.com/" + owner + "/" + repo + "/pull/" + strconv.Itoa(n)}
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
		pr:          prIdentity(42, "o", "r"),
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
	// The capability path REPLACES OpenRequest. A separate counter is what
	// makes this assertion able to fail: a shared `opened` flag could not.
	if ad.openCalls != 0 {
		t.Errorf("OpenRequest was ALSO called %d times on the capability path — that opens two PRs", ad.openCalls)
	}
	// The task's repo ref reaches the adapter as host-matching material.
	if ad.gotReq.RepoRef != "https://github.com/o/r.git" {
		t.Errorf("request RepoRef = %q, want the task's repo ref", ad.gotReq.RepoRef)
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
	// The watch coordinate is stored as explicit VALIDATED fields, so the
	// watcher never re-parses the URL to recover it.
	if m.PROwner != "o" || m.PRRepo != "r" {
		t.Errorf("marker pr_owner/pr_repo = %q/%q, want o/r", m.PROwner, m.PRRepo)
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
		pr:          prIdentity(42, "o", "r"),
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
		pr:          prIdentity(7, "o", "r"),
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
		pr:          prIdentity(99, "o", "r"),
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

// A capability that returns BOTH an error and a non-zero identity must be
// treated as a failed open. Emitting pr_opened:false alongside pr_number:N
// would be a self-contradicting terminal record, and starting a watch on a PR
// the broker just reported it failed to open is worse than either half alone.
func TestFinishPush_ErrorWithIdentity_IsAFailedOpen(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          prIdentity(42, "o", "r"), // a contradictory implementation
		resultErr:   errors.New("gh: exit status 1"),
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed (the branch landed)", term["outcome"])
	}
	if term["pr_opened"] != false {
		t.Errorf("pr_opened = %v, want false — the error wins", term["pr_opened"])
	}
	if _, ok := term["pr_number"]; ok {
		t.Errorf("pr_number = %v present alongside pr_opened:false — the event contradicts itself", term["pr_number"])
	}
	if _, ok := term["pr_url"]; ok {
		t.Error("pr_url must be absent when the open reported an error")
	}
	if files := markerFiles(t, b.AuditRoot); len(files) != 0 {
		t.Errorf("a watch was started on a PR the broker reported it failed to open: %v", files)
	}
}

// An embedded clone credential must never land in the durable marker — the
// same rule the Brief enforces with RedactRepoRef, and doubly here because the
// marker's contents become a `gh --repo` argv where userinfo is visible in `ps`.
//
// gitURLRef already refuses a credential-bearing repo_ref at the HTTP/queue
// boundary, so this drives recordCIMarker directly — the same reason the Brief
// redacts rather than trusting an upstream check, and the reason the redaction
// belongs at the WRITE, where the durable bytes are produced.
func TestFinishPush_MarkerRepoRefIsRedacted(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          prIdentity(42, "o", "r"),
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	id := "fedcba9876543210fedcba9876543210"
	tr := &taskRun{b: b, id: id, repoRef: "https://alice:ghp_supersecret@github.com/o/r.git"}
	tr.recordCIMarker("agent/"+id, prIdentity(42, "o", "r"))

	m, err := readCIMarker(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("readCIMarker: %v", err)
	}
	if strings.Contains(m.RepoRef, "ghp_supersecret") || strings.Contains(m.RepoRef, "alice") {
		t.Errorf("marker repo_ref leaked the clone credential: %q", m.RepoRef)
	}
	if m.RepoRef != "https://github.com/o/r.git" {
		t.Errorf("marker repo_ref = %q, want the redacted ref", m.RepoRef)
	}
	// Redaction must not break the host gate the watcher applies.
	if !ciRefHostWatchable(m.RepoRef) {
		t.Error("the redacted ref is no longer recognizable as a github.com repo")
	}
	// And the whole file, not just the field.
	raw, err := os.ReadFile(filepath.Join(b.AuditRoot, id+".ci.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ghp_supersecret") {
		t.Errorf("the credential is present somewhere in the marker file: %s", raw)
	}
}

// The fork case end to end: the task cloned myfork/proj, the PR was opened on
// upstream/proj, and the marker records the PR's OWN owner/repo — that is
// where the checks live, and it is what the watcher will query.
func TestFinishPush_ForkPR_MarkerCarriesThePRsOwnRepo(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          prIdentity(8, "upstream", "proj"),
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	body := `{"repo_ref":"https://github.com/myfork/proj.git","instruction":"do x","agent":"claude","auto_approve":true}`
	_, _, term := submit(b, body)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed", term["outcome"])
	}
	id, _ := term["task_id"].(string)
	m, err := readCIMarker(b.AuditRoot, id)
	if err != nil {
		t.Fatalf("readCIMarker: %v", err)
	}
	if m.PROwner != "upstream" || m.PRRepo != "proj" {
		t.Errorf("marker pr_owner/pr_repo = %q/%q, want the PR's own upstream/proj", m.PROwner, m.PRRepo)
	}
	if m.RepoRef != "https://github.com/myfork/proj.git" {
		t.Errorf("marker repo_ref = %q, want the TASK's repo (both are recorded)", m.RepoRef)
	}
	// The watch queries the PR's repo, not the task's.
	owner, repo := m.PROwner, m.PRRepo
	if owner == "myfork" {
		t.Error("the watch would query the fork, where the PR's checks do not live")
	}
	if !remote.ValidOwnerRepo(owner) || !remote.ValidOwnerRepo(repo) {
		t.Errorf("marker owner/repo did not survive validation: %q/%q", owner, repo)
	}
}

// pr_url is subprocess-derived text reflected to an operator terminal: it goes
// through safeStr (control-character stripping + a length cap) like every other
// reflected string, regardless of what upstream validation already did.
func TestFinishPush_PRURLIsSanitizedInTheEvent(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	hostile := "https://github.com/o/r/pull/42\x1b[2J\x07" + strings.Repeat("A", 500)
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          remote.PullRequest{Number: 42, Owner: "o", Repo: "r", URL: hostile},
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	_, _, term := submit(b, pushBody)
	got, _ := term["pr_url"].(string)
	if got == hostile {
		t.Fatal("pr_url was reflected raw")
	}
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("pr_url still carries control characters: %q", got)
	}
	if len(got) > 210 {
		t.Errorf("pr_url is %d bytes, want it capped", len(got))
	}
}

// An identity with no validated owner/repo cannot be watched — the watcher
// builds its argv from those fields and may not re-derive them from the URL —
// so no marker is written. The push still succeeds and still reports the PR.
func TestFinishPush_IdentityWithoutOwnerRepo_NoMarker(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a b\n+x"}
	ad := &capturingAdapter{
		fakeAdapter: fakeAdapter{name: "github"},
		pr:          remote.PullRequest{Number: 42, URL: "https://github.com/o/r/pull/42"},
	}
	b := ciTestBroker(t, st, ad)
	b.CIWatch = true

	_, _, term := submit(b, pushBody)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome = %v, want pushed", term["outcome"])
	}
	if term["pr_number"] != float64(42) {
		t.Errorf("pr_number = %v, want 42 (the identity is still reported)", term["pr_number"])
	}
	if files := markerFiles(t, b.AuditRoot); len(files) != 0 {
		t.Errorf("a marker was written with no watchable coordinate: %v", files)
	}
}
