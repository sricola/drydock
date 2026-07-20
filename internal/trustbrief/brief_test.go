package trustbrief

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleBrief() Brief {
	return Brief{
		SchemaVersion: 1,
		TaskID:        "0123456789abcdef0123456789abcdef",
		GeneratedAt:   time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		Task: TaskFacts{
			InstructionSHA256: HashInstruction("fix the race"),
			RepoRef:           "https://github.com/o/r.git",
			BaseCommit:        "deadbeef",
			Sensitive:         true,
		},
		Runtime: RuntimeFacts{ImageRef: "drydock-sandbox:latest", Agent: "claude", Vendor: "anthropic", Model: "model-x"},
		Policy: PolicyFacts{
			BudgetUSD: 2, TimeoutSeconds: 1800,
			EgressDefault: []string{"api.anthropic.com:443"},
			EgressWidened: []string{"api.example.com:443"},
		},
		Spend:           SpendFacts{USDBrokerMetered: 0.12, DurationMs: 60000},
		Diff:            Analyze("diff --git a/x b/x\n+y\n"),
		Verification:    Verification{Status: VerificationNotConfigured},
		MissingEvidence: []string{"verification not configured"},
	}
}

func TestWriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	b := sampleBrief()
	b.Policy.SnapshotSHA256 = b.Policy.Fingerprint()
	if err := Write(dir, b.TaskID, b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, b.TaskID+Suffix))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	got, err := Read(dir, b.TaskID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, b) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, b)
	}
}

// M3: a corrupt brief file must fail with an error that names the "parse"
// step, so callers (cmd/drydock review.go) can distinguish "brief absent"
// (os.IsNotExist) from "brief present but corrupt" (the callers' concern) and
// warn instead of silently proceeding as if there were no evidence at all.
func TestRead_CorruptJSON_ErrorMentionsParse(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dir, id+Suffix), []byte("not json at all {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(dir, id)
	if err == nil {
		t.Fatal("Read of corrupt JSON succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("Read error = %q, want it to mention %q", err.Error(), "parse")
	}
}

func TestRead_RejectsHostileIDs(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"../../etc/passwd", "abc", "ABCDEF6789abcdef0123456789abcdef", ""} {
		if _, err := Read(dir, id); err == nil {
			t.Errorf("Read(%q) succeeded, want id-shape rejection", id)
		}
	}
}

func TestWrite_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, id+Suffix)); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, id, sampleBrief()); err == nil {
		t.Error("Write through a planted symlink succeeded, want O_NOFOLLOW failure")
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep" {
		t.Error("symlink target was overwritten")
	}
}

func TestPolicyFingerprint_StableAndSelfExcluding(t *testing.T) {
	p := sampleBrief().Policy
	f1 := p.Fingerprint()
	p.SnapshotSHA256 = f1
	if f2 := p.Fingerprint(); f2 != f1 {
		t.Errorf("Fingerprint changed after embedding itself: %s != %s", f2, f1)
	}
	p.BudgetUSD = 3
	if p.Fingerprint() == f1 {
		t.Error("Fingerprint identical after a policy change")
	}
	if len(f1) != 64 {
		t.Errorf("Fingerprint = %q, want 64 hex chars", f1)
	}
}

func TestRedactRepoRef(t *testing.T) {
	cases := map[string]string{
		"https://user:sekret@github.com/o/r.git": "https://github.com/o/r.git",
		"https://github.com/o/r.git":             "https://github.com/o/r.git",
		"git@github.com:o/r.git":                 "git@github.com:o/r.git",
	}
	for in, want := range cases {
		if got := RedactRepoRef(in); got != want {
			t.Errorf("RedactRepoRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Brief must be credential-free by construction. Two tripwires: no field
// in the struct tree may be named like a secret carrier, and a marshaled
// sample must not contain secret-shaped strings.
func TestBrief_NoSecretShapedFields(t *testing.T) {
	var check func(t *testing.T, typ reflect.Type, path string)
	check = func(t *testing.T, typ reflect.Type, path string) {
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				lower := strings.ToLower(f.Name)
				for _, bad := range []string{"token", "secret", "password", "credential", "apikey"} {
					if strings.Contains(lower, bad) {
						t.Errorf("field %s.%s is secret-shaped; the Brief must stay credential-free", path, f.Name)
					}
				}
				check(t, f.Type, path+"."+f.Name)
			}
		case reflect.Slice, reflect.Ptr, reflect.Array:
			check(t, typ.Elem(), path)
		}
	}
	check(t, reflect.TypeOf(Brief{}), "Brief")

	out, err := json.Marshal(sampleBrief())
	if err != nil {
		t.Fatal(err)
	}
	for _, pat := range []string{"tok_", "sk-ant", "ghp_", "Bearer "} {
		if strings.Contains(string(out), pat) {
			t.Errorf("marshaled brief contains secret-shaped string %q", pat)
		}
	}
}
