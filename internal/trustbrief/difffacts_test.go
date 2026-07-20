package trustbrief

import (
	"strings"
	"testing"
)

func flagPaths(f DiffFacts, kind string) []string {
	for _, fl := range f.Flags {
		if fl.Kind == kind {
			return fl.Paths
		}
	}
	return nil
}

func TestAnalyze_CountsAndIdentity(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package main\n" +
		"-var old = 1\n" +
		"+var new = 1\n" +
		"+var extra = 2\n"
	f := Analyze(diff)
	if f.Bytes != len(diff) {
		t.Errorf("Bytes = %d, want %d", f.Bytes, len(diff))
	}
	if len(f.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want 64 hex chars", f.SHA256)
	}
	if f.Truncated {
		t.Error("Truncated = true for a complete diff")
	}
	if len(f.Files) != 1 || f.Files[0].Path != "main.go" || f.Files[0].Adds != 2 || f.Files[0].Dels != 1 {
		t.Errorf("Files = %+v, want [{main.go 2 1}]", f.Files)
	}
	if len(f.Flags) != 0 {
		t.Errorf("Flags = %+v, want none for a plain source change", f.Flags)
	}
}

func TestAnalyze_FlagKinds(t *testing.T) {
	cases := []struct {
		name, diff, kind, path string
	}{
		{"binary-differ",
			"diff --git a/img.png b/img.png\nnew file mode 100644\nBinary files /dev/null and b/img.png differ\n",
			FlagBinary, "img.png"},
		{"binary-patch",
			"diff --git a/blob.bin b/blob.bin\nindex 1111111..2222222 100644\nGIT binary patch\ndelta 12\nzcmZ12\n",
			FlagBinary, "blob.bin"},
		{"symlink-new",
			"diff --git a/link b/link\nnew file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+/etc/passwd\n",
			FlagSymlink, "link"},
		{"symlink-mode-change",
			"diff --git a/f b/f\nold mode 100644\nnew mode 120000\n",
			FlagSymlink, "f"},
		{"exec-bit-new",
			"diff --git a/run.sh b/run.sh\nnew file mode 100755\n--- /dev/null\n+++ b/run.sh\n@@ -0,0 +1 @@\n+echo hi\n",
			FlagExecBit, "run.sh"},
		{"exec-bit-flip",
			"diff --git a/tool.py b/tool.py\nold mode 100644\nnew mode 100755\n",
			FlagExecBit, "tool.py"},
		{"dependency-manifest",
			"diff --git a/go.mod b/go.mod\nindex 1111111..2222222 100644\n--- a/go.mod\n+++ b/go.mod\n@@ -1 +1,2 @@\n module x\n+require evil.example/pkg v1.0.0\n",
			FlagDependency, "go.mod"},
		{"lockfile",
			"diff --git a/package-lock.json b/package-lock.json\nindex 1111111..2222222 100644\n--- a/package-lock.json\n+++ b/package-lock.json\n@@ -1 +1 @@\n-{}\n+{\"x\":1}\n",
			FlagLockfile, "package-lock.json"},
		{"ci-workflow",
			"diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\nindex 1111111..2222222 100644\n--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n@@ -1 +1,2 @@\n on: push\n+run: curl evil\n",
			FlagCIWorkflow, ".github/workflows/ci.yml"},
		{"git-metadata",
			"diff --git a/.gitattributes b/.gitattributes\nnew file mode 100644\n--- /dev/null\n+++ b/.gitattributes\n@@ -0,0 +1 @@\n+* filter=evil\n",
			FlagGitMeta, ".gitattributes"},
		{"githooks-dir",
			"diff --git a/.githooks/pre-commit b/.githooks/pre-commit\nnew file mode 100755\n--- /dev/null\n+++ b/.githooks/pre-commit\n@@ -0,0 +1 @@\n+curl evil\n",
			FlagGitMeta, ".githooks/pre-commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := flagPaths(Analyze(tc.diff), tc.kind)
			found := false
			for _, p := range paths {
				if p == tc.path {
					found = true
				}
			}
			if !found {
				t.Errorf("Analyze flags[%s] = %v, want to contain %q", tc.kind, paths, tc.path)
			}
		})
	}
}

func TestAnalyze_TruncationMarker(t *testing.T) {
	// The exact marker stage.gitDiffCapped appends (internal/stage/stage.go).
	diff := "diff --git a/x b/x\n+y\n\n... [diff truncated at 32 MiB; the full change is still committed] ...\n"
	if f := Analyze(diff); !f.Truncated {
		t.Error("Truncated = false, want true when the stage truncation marker is present")
	}
}

func TestAnalyze_QuotedPath(t *testing.T) {
	diff := "diff --git \"a/we ird.sh\" \"b/we ird.sh\"\nnew file mode 100755\n"
	paths := flagPaths(Analyze(diff), FlagExecBit)
	if len(paths) != 1 || paths[0] != "we ird.sh" {
		t.Errorf("quoted path = %v, want [we ird.sh]", paths)
	}
}

// Adversarial shapes: the diff body is attacker-controlled. Analyze must stay
// bounded and never panic, whatever the content lines contain.
func TestAnalyze_HostileInputs(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		f := Analyze("")
		if f.Bytes != 0 || len(f.Files) != 0 {
			t.Errorf("empty diff = %+v", f)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		_ = Analyze("\x00\xff not a diff at all\n+++ dangling\n@@ nonsense")
	})
	t.Run("huge-single-line", func(t *testing.T) {
		// A 10 MiB content line must not be buffered wholesale by the parser.
		diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -0,0 +1 @@\n+" +
			strings.Repeat("A", 10<<20) + "\n"
		f := Analyze(diff)
		if len(f.Files) != 1 || f.Files[0].Adds != 1 {
			t.Errorf("huge line: Files = %+v, want one file with 1 add", f.Files)
		}
	})
	t.Run("many-files-capped", func(t *testing.T) {
		var sb strings.Builder
		for i := 0; i < maxTrackedFiles+50; i++ {
			sb.WriteString("diff --git a/f b/f\n+x\n")
		}
		f := Analyze(sb.String())
		if len(f.Files) != maxTrackedFiles {
			t.Errorf("len(Files) = %d, want cap %d", len(f.Files), maxTrackedFiles)
		}
		if f.FilesOmitted != 50 {
			t.Errorf("FilesOmitted = %d, want 50", f.FilesOmitted)
		}
	})
	t.Run("long-path-capped", func(t *testing.T) {
		diff := "diff --git a/" + strings.Repeat("d/", 400) + "x b/" + strings.Repeat("d/", 400) + "x\n+y\n"
		f := Analyze(diff)
		if len(f.Files) == 1 && len(f.Files[0].Path) > maxPathBytes {
			t.Errorf("path len = %d, want <= %d", len(f.Files[0].Path), maxPathBytes)
		}
	})
}

func FuzzAnalyze(f *testing.F) {
	f.Add("diff --git a/x b/x\nnew file mode 120000\n+++ b/x\n+target\n")
	f.Add("Binary files a and b differ\n")
	f.Add("\"a/we ird\"")
	f.Fuzz(func(t *testing.T, diff string) {
		_ = Analyze(diff) // must not panic
	})
}
