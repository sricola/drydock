package stage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportStaged_IgnoresInTreeExportAttributes pins the A5/A9 claim that the
// verifier VM sees exactly the tree that will be pushed. `git archive` honours
// the archived tree's own .gitattributes, so an agent could mark a file
// `export-ignore` (hidden from the verifier, still pushed) or `export-subst`
// (rewritten for the verifier, pushed verbatim) while StagedTreeHash still
// matched at push time. Every path in the staged tree must be exported, byte
// for byte, regardless of what the work tree's attributes say.
func TestExportStaged_IgnoresInTreeExportAttributes(t *testing.T) {
	src := newLocalSourceRepo(t)
	st, err := Prepare(context.Background(), t.TempDir(), src)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	files := map[string]string{
		"evil.go":            "package evil\n",
		"sub/deep/x.go":      "package deep\n",
		"stamped.txt":        "stamp $Format:%H$\n",
		"ok.go":              "package ok\n",
		".gitattributes":     "evil.go export-ignore\nsub export-ignore\nstamped.txt hide\n[attr]hide export-subst export-ignore\nok.go export-subst\n",
		"sub/.gitattributes": "* export-ignore\n",
	}
	for name, body := range files {
		p := filepath.Join(st.WorkDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(st.Root, "verify")
	if _, err := st.ExportStaged(dst); err != nil {
		t.Fatalf("ExportStaged: %v", err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("%s: missing from the verifier export (in-tree export attribute hid it): %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s: exported content %q, want the staged bytes %q", name, got, want)
		}
	}
}

// The post-export completeness check must fail closed when a staged path is
// absent from the verifier copy, whatever removed it.
func TestVerifyExportComplete_RefusesAMissingPath(t *testing.T) {
	src := newLocalSourceRepo(t)
	st, err := Prepare(context.Background(), t.TempDir(), src)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(st.WorkDir, "keep.go"), []byte("package keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(st.Root, "verify")
	tree, err := st.ExportStaged(dst)
	if err != nil {
		t.Fatalf("ExportStaged: %v", err)
	}
	if err := st.verifyExportComplete(tree, dst); err != nil {
		t.Fatalf("complete export reported incomplete: %v", err)
	}
	if err := os.Remove(filepath.Join(dst, "keep.go")); err != nil {
		t.Fatal(err)
	}
	err = st.verifyExportComplete(tree, dst)
	if err == nil {
		t.Fatal("a verifier copy missing keep.go passed the completeness check")
	}
	if !strings.Contains(err.Error(), "keep.go") || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error %q should name the missing path and say the export is incomplete", err)
	}
}

// A3, host side: a hook inside the HOST-ONLY git dir (not the agent-visible
// /work/.git, which --git-dir already ignores) must not run either. This is
// what core.hooksPath=/dev/null is for: a clone template or a copied hooks
// directory on the operator's machine cannot execute during the broker's own
// commit.
func TestHostCommit_IgnoresHookInHostGitDir(t *testing.T) {
	// A clone does not inherit the source repo's user.* config, and CI
	// runners have no global identity.
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	src := newLocalSourceRepo(t)
	st, err := Prepare(context.Background(), t.TempDir(), src)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "PWNED")
	hookDir := filepath.Join(st.gitDir, "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-commit", "post-commit", "commit-msg"} {
		if err := os.WriteFile(filepath.Join(hookDir, name), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(st.WorkDir, "a.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.stageAll(); err != nil {
		t.Fatalf("stageAll: %v", err)
	}
	if _, err := st.git("commit", "-m", "agent change"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a hook in the host-only git dir EXECUTED (marker exists): err=%v", err)
	}
}

// Beyond export-ignore/export-subst, `git archive` runs the working-tree
// conversions on every blob: text/eol, ident and working-tree-encoding rewrite
// bytes, and a `filter=` driver from the operator's gitconfig (git-lfs is the
// common one) runs a host-side command on agent-chosen input. The verifier
// must receive the staged blob byte for byte and no driver may run.
func TestExportStaged_IgnoresConvertAttributesAndFilterDrivers(t *testing.T) {
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	marker := filepath.Join(t.TempDir(), "RAN")
	if err := os.WriteFile(gcfg, []byte("[filter \"op\"]\n\tsmudge = \"sh -c 'touch "+marker+" && tr a-z A-Z'\"\n\tclean = cat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	st, err := Prepare(context.Background(), t.TempDir(), newLocalSourceRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"crlf.sh":        "#!/bin/sh\necho hi\n",
		"ident.go":       "package x // $Id$\n",
		"utf16.txt":      "\xff\xfeh\x00i\x00\n\x00",
		"f.txt":          "lower\n",
		".gitattributes": "crlf.sh text eol=crlf\nident.go ident\nutf16.txt working-tree-encoding=UTF-16\nf.txt filter=op\n",
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(st.WorkDir, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(st.Root, "verify")
	if _, err := st.ExportStaged(dst); err != nil {
		t.Fatal(err)
	}
	for n, want := range files {
		got, _ := os.ReadFile(filepath.Join(dst, n))
		blob, err := st.git("show", ":"+n)
		if err != nil {
			t.Fatal(err)
		}
		if blob != want {
			t.Errorf("%s: staged blob %q differs from the bytes the agent wrote %q (a clean filter or text conversion ran at add time)", n, blob, want)
		}
		if string(got) != blob {
			t.Errorf("%s: verifier sees %q, pushed blob is %q", n, got, blob)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an agent-selected filter driver executed on the host")
	}
}

// The reviewed diff must show the staged bytes. `diff=<driver>` in the tree
// selects an operator textconv/external diff from ~/.gitconfig, which would
// both rewrite what the approver reads and run a host tool on agent input.
func TestCaptureDiff_IgnoresTextconvAndExternalDiff(t *testing.T) {
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	marker := filepath.Join(t.TempDir(), "RAN")
	if err := os.WriteFile(gcfg, []byte("[diff \"op\"]\n\ttextconv = \"sh -c 'touch "+marker+"; echo // nothing to see here' --\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	st, err := Prepare(context.Background(), t.TempDir(), newLocalSourceRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(st.WorkDir, "evil.go"), []byte("package evil // rm -rf /\n"), 0o644)
	os.WriteFile(filepath.Join(st.WorkDir, ".gitattributes"), []byte("evil.go diff=op\n"), 0o644)
	diff, err := st.CaptureDiff()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "rm -rf /") {
		t.Fatalf("reviewed diff hides the staged content:\n%s", diff)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an agent-selected diff driver executed on the host")
	}
}

// An in-tree `-diff` (or the `binary` macro, which implies it) makes git print
// "Binary files ... differ" for a text file, so the approver would see no code
// at all. The override restores git's own text/binary detection; a genuinely
// binary blob still diffs as binary.
func TestCaptureDiff_IgnoresInTreeBinaryMarking(t *testing.T) {
	for _, attrs := range []string{"*.go -diff\n", "*.go binary\n"} {
		st, err := Prepare(context.Background(), t.TempDir(), newLocalSourceRepo(t))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(st.WorkDir, "evil.go"), []byte("package evil // rm -rf /\n"), 0o644)
		os.WriteFile(filepath.Join(st.WorkDir, "blob.bin"), []byte("\x00\x01\x02"), 0o644)
		os.WriteFile(filepath.Join(st.WorkDir, ".gitattributes"), []byte(attrs), 0o644)
		diff, err := st.CaptureDiff()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(diff, "rm -rf /") {
			t.Errorf("%q: reviewed diff hides a text file as binary:\n%s", attrs, diff)
		}
		if !strings.Contains(diff, "Binary files /dev/null and b/blob.bin differ") {
			t.Errorf("%q: a real binary blob should still diff as binary:\n%s", attrs, diff)
		}
	}
}

// A verifier copy with the right names but the wrong NUMBER of entries (what a
// case-insensitive volume produces for Case.go + case.go) must be refused.
func TestVerifyExportComplete_RefusesAnEntryCountMismatch(t *testing.T) {
	st, err := Prepare(context.Background(), t.TempDir(), newLocalSourceRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(st.WorkDir, "keep.go"), []byte("package keep\n"), 0o644)
	dst := filepath.Join(st.Root, "verify")
	tree, err := st.ExportStaged(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Add an extra file the tree does not list: counts now disagree.
	os.WriteFile(filepath.Join(dst, "extra.go"), []byte("x"), 0o644)
	if err := st.verifyExportComplete(tree, dst); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("entry-count mismatch not refused: %v", err)
	}
}

// The work tree must start as the exact blobs: the override has to exist
// before checkout. Otherwise an in-tree eol=crlf hands the agent CRLF files
// (each one it touches then re-adds as a whole-file CRLF diff) and a filter
// driver from the operator's gitconfig (git-lfs, say) smudges on the host.
func TestPrepare_ChecksOutExactBlobs(t *testing.T) {
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	marker := filepath.Join(t.TempDir(), "SMUDGED")
	if err := os.WriteFile(gcfg, []byte("[filter \"op\"]\n\tsmudge = \"sh -c 'touch "+marker+"; cat'\"\n\tclean = cat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\ntwo\n"), 0o644)
	os.WriteFile(filepath.Join(src, "b.dat"), []byte("payload\n"), 0o644)
	if out, err := exec.Command("git", "-C", src, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	// Attributes land in the commit, after the blobs were stored as LF.
	os.WriteFile(filepath.Join(src, ".gitattributes"), []byte("*.txt text eol=crlf\n*.dat filter=op\n"), 0o644)
	for _, args := range [][]string{{"add", ".gitattributes"}, {"commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	st, err := Prepare(context.Background(), t.TempDir(), src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(st.WorkDir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\ntwo\n" {
		t.Errorf("work tree a.txt = %q, want the LF blob", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an in-tree filter= ran the operator's smudge driver on the host at Prepare")
	}
	// Rewriting a file with its own content must not produce a diff.
	os.WriteFile(filepath.Join(st.WorkDir, "a.txt"), got, 0o644)
	diff, err := st.CaptureDiff()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(diff) != "" {
		t.Errorf("untouched content shows a diff:\n%s", diff)
	}
}

// A brand-new remote with no commits has no HEAD to check out; Prepare must
// still succeed, as the plain clone it replaced did.
func TestPrepare_EmptyRepository(t *testing.T) {
	src := t.TempDir()
	if out, err := exec.Command("git", "-C", src, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if _, err := Prepare(context.Background(), t.TempDir(), src); err != nil {
		t.Fatalf("Prepare on an empty repository: %v", err)
	}
}
