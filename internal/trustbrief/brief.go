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

// VerificationNotConfigured is the v1 status: no verifier stage exists yet.
// The field is present from schema v1 so adding the verifier is a data
// change, not a schema migration.
const VerificationNotConfigured = "not_configured"

// Brief is the broker-observed evidence report for one task, generated at
// the diff-approval gate. Every field is computed host-side; by design the
// schema has nowhere to put an agent claim.
type Brief struct {
	SchemaVersion   int          `json:"schema_version"`
	TaskID          string       `json:"task_id"`
	GeneratedAt     time.Time    `json:"generated_at"`
	Task            TaskFacts    `json:"task"`
	Runtime         RuntimeFacts `json:"runtime"`
	Policy          PolicyFacts  `json:"policy"`
	Spend           SpendFacts   `json:"spend"`
	Diff            DiffFacts    `json:"diff"`
	Verification    Verification `json:"verification"`
	MissingEvidence []string     `json:"missing_evidence"`
}

type TaskFacts struct {
	InstructionSHA256 string `json:"instruction_sha256"`
	RepoRef           string `json:"repo_ref"`
	BaseCommit        string `json:"base_commit,omitempty"`
	Sensitive         bool   `json:"sensitive"`
	AutoApprove       bool   `json:"auto_approve"`
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
	SnapshotSHA256 string   `json:"snapshot_sha256"`
	BudgetUSD      float64  `json:"budget_usd"`
	BudgetHard     bool     `json:"budget_hard"`
	MaxRequests    int      `json:"max_requests"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	EgressDefault  []string `json:"egress_default"`
	EgressWidened  []string `json:"egress_widened"`
}

type SpendFacts struct {
	USDBrokerMetered float64 `json:"usd_broker_metered"`
	DurationMs       int64   `json:"duration_ms"`
}

type Verification struct {
	Status string `json:"status"`
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
