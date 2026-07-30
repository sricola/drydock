package trustbrief

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Suffix is the audit-dir filename suffix for persisted briefs, alongside
// the existing .jsonl/.diff/.widen.json per-task artifacts.
const Suffix = ".brief.json"

// VerificationNotConfigured means no verify.repos entry matched this task's
// repository, so the broker ran no verifier stage for it. The field has been
// present since schema v1 (predating the verifier), so adding the verifier
// was a data change, not a schema migration.
const VerificationNotConfigured = "not_configured"

// Overall verification statuses. "inconclusive" covers timeouts and infra
// errors — evidence is absent, which must never read as passing.
const (
	VerificationPassed       = "passed"
	VerificationFailed       = "failed"
	VerificationInconclusive = "inconclusive"
)

// Per-command statuses.
const (
	VerifyCmdPassed   = "passed"
	VerifyCmdFailed   = "failed"
	VerifyCmdTimedOut = "timed_out"
	VerifyCmdError    = "error"
	VerifyCmdSkipped  = "skipped"
)

// VerifyCommand is one verification command's broker-observed result. The
// exit code is read from the container process by the broker; nothing in
// the verifier VM's output can influence these fields.
type VerifyCommand struct {
	Argv       []string `json:"argv"`
	Status     string   `json:"status"`
	ExitCode   int      `json:"exit_code"`
	DurationMs int64    `json:"duration_ms"`
}

// Verification is the independent-verifier evidence block. Network and
// Credentials record the verifier VM's capability posture ("denied"/"none");
// TreeSHA is the staged tree the commands ran against — the same tree the
// push-time guard re-checks so pushed tree == verified tree.
type Verification struct {
	Status      string          `json:"status"`
	Network     string          `json:"network,omitempty"`
	Credentials string          `json:"credentials,omitempty"`
	TreeSHA     string          `json:"tree_sha,omitempty"`
	LogSHA256   string          `json:"log_sha256,omitempty"`
	Commands    []VerifyCommand `json:"commands,omitempty"`
}

// Setup statuses. SetupNotConfigured means the execution profile declared no
// setup phase. "inconclusive" covers timeouts and infra errors — absent
// evidence must never read as passing, mirroring verification.
const (
	SetupNotConfigured = "not_configured"
	SetupPassed        = "passed"
	SetupFailed        = "failed"
	SetupInconclusive  = "inconclusive"
)

// SetupCommand is one setup command's broker-observed result. Per-command
// statuses reuse the VerifyCmd* consts — the vocabulary (passed/failed/
// timed_out/error/skipped) is identical.
type SetupCommand struct {
	Argv       []string `json:"argv"`
	Status     string   `json:"status"`
	ExitCode   int      `json:"exit_code"`
	DurationMs int64    `json:"duration_ms"`
}

// SetupEvidence is the setup-phase evidence block, mirroring Verification.
// Network records the setup container's egress posture ("egress-allowlisted"
// — setup does have network access, unlike the verifier's "denied").
type SetupEvidence struct {
	Status    string         `json:"status"`
	Network   string         `json:"network,omitempty"`
	LogSHA256 string         `json:"log_sha256,omitempty"`
	Commands  []SetupCommand `json:"commands,omitempty"`
}

// Brief is the broker-observed evidence report for one task, generated at
// the diff-approval gate. Every field is computed host-side; by design the
// schema has nowhere to put an agent claim.
type Brief struct {
	SchemaVersion   int           `json:"schema_version"`
	TaskID          string        `json:"task_id"`
	GeneratedAt     time.Time     `json:"generated_at"`
	Task            TaskFacts     `json:"task"`
	Runtime         RuntimeFacts  `json:"runtime"`
	Policy          PolicyFacts   `json:"policy"`
	Spend           SpendFacts    `json:"spend"`
	Diff            DiffFacts     `json:"diff"`
	Verification    Verification  `json:"verification"`
	Setup           SetupEvidence `json:"setup"`
	MissingEvidence []string      `json:"missing_evidence"`
}

// TaskFacts records the host-observed facts of the request itself. PlanOnly
// and IssueURL (both additive, omitempty — no SchemaVersion bump) carry
// plan-mode provenance: PlanOnly marks a run whose terminal was "planned"
// (the broker never verified or pushed), and IssueURL records the issue the
// instruction was ingested from, both broker-side fields — never agent
// claims.
type TaskFacts struct {
	InstructionSHA256 string `json:"instruction_sha256"`
	RepoRef           string `json:"repo_ref"`
	BaseCommit        string `json:"base_commit,omitempty"`
	Sensitive         bool   `json:"sensitive"`
	AutoApprove       bool   `json:"auto_approve"`
	PlanOnly          bool   `json:"plan_only,omitempty"`
	IssueURL          string `json:"issue_url,omitempty"`
}

type RuntimeFacts struct {
	ImageRef string `json:"image_ref"`
	Agent    string `json:"agent"`
	Vendor   string `json:"vendor"`
	Model    string `json:"model,omitempty"`
}

// PolicyFacts is the effective policy that bounded this task. BudgetHard
// records whether per-request reservation (max_request_cost_usd > 0) made
// the USD cap a hard ceiling — without it the cap is post-hoc/soft, and the
// reviewer should know which guarantee they got.
type PolicyFacts struct {
	SnapshotSHA256 string  `json:"snapshot_sha256"`
	BudgetUSD      float64 `json:"budget_usd"`
	BudgetHard     bool    `json:"budget_hard"`
	// BudgetUnbounded is true when this task's vendor lane carries no USD
	// metering at all (subscription auth; a priceless openai-compat lane).
	// When set, BudgetUSD is 0 and BudgetHard is false — the real backstop is
	// MaxRequests, not a dollar figure that was never actually enforced.
	BudgetUnbounded bool     `json:"budget_unbounded"`
	MaxRequests     int      `json:"max_requests"`
	TimeoutSeconds  int      `json:"timeout_seconds"`
	EgressDefault   []string `json:"egress_default"`
	EgressWidened   []string `json:"egress_widened"`
}

type SpendFacts struct {
	USDBrokerMetered float64 `json:"usd_broker_metered"`
	DurationMs       int64   `json:"duration_ms"`
}

// HashInstruction returns the sha256 of the task instruction. The Brief
// stores the hash, not the text: the instruction may embed sensitive
// context, and the full text is already in the 0600 audit trace for those
// who need it.
func HashInstruction(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// RedactRepoRef strips userinfo from URL-shaped repo refs so an embedded
// clone credential (https://user:pass@host/...) never lands in the Brief.
// scp-style refs (git@host:path) carry no password and pass through.
func RedactRepoRef(ref string) string {
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.User != nil {
		u.User = nil
		return u.String()
	}
	return ref
}

// Fingerprint hashes the policy block (with the embedded hash field zeroed)
// so briefs, `policy explain`, and a future /admin/policy endpoint can all
// agree on one policy identity string.
func (p PolicyFacts) Fingerprint() string {
	p.SnapshotSHA256 = ""
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// taskIDRE is the task-id grammar (matches the broker's 128-bit hex ids and
// the webui's validation). Read builds a path from its id argument, so the
// shape check is what keeps a hostile id from traversing out of the dir.
var taskIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Write persists the brief as <dir>/<id>.brief.json with the same defenses
// as the other audit artifacts: 0600, O_NOFOLLOW (a planted symlink fails
// instead of redirecting the write).
func Write(dir, id string, b Brief) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+Suffix),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// Read loads a persisted brief. The id must match the task-id grammar —
// callers pass operator-typed input straight in.
func Read(dir, id string) (Brief, error) {
	if !taskIDRE.MatchString(id) {
		return Brief{}, fmt.Errorf("trustbrief: invalid task id %q", id)
	}
	f, err := os.OpenFile(filepath.Join(dir, id+Suffix), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Brief{}, err
	}
	defer f.Close()
	var b Brief
	if err := json.NewDecoder(f).Decode(&b); err != nil {
		return Brief{}, fmt.Errorf("trustbrief: parse %s%s: %w", id, Suffix, err)
	}
	return b, nil
}
