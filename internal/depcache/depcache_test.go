package depcache

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// baseKey returns a fully-populated Key; tests mutate copies of it.
func baseKey() Key {
	return Key{
		RepoKey: "github.com/acme/widget",
		LockDigests: []string{
			"go.sum:" + strings.Repeat("aa", 32),
			"package-lock.json:" + strings.Repeat("bb", 32),
		},
		Toolchain:      "go1.26.5",
		Arch:           "arm64",
		SetupCmdDigest: strings.Repeat("cc", 32),
		ImageRef:       "drydock-sandbox:v0.6.7",
	}
}

func mustKey(t *testing.T, k Key) string {
	t.Helper()
	s, err := ComputeKey(k)
	if err != nil {
		t.Fatalf("ComputeKey(%+v): %v", k, err)
	}
	return s
}

func TestComputeKey_StableAndInjective(t *testing.T) {
	base := mustKey(t, baseKey())

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(base) {
		t.Fatalf("key %q is not 64 lowercase hex", base)
	}

	// Deterministic: same inputs, same key.
	if again := mustKey(t, baseKey()); again != base {
		t.Errorf("same inputs produced different keys: %s vs %s", base, again)
	}

	// Order-insensitive over lock digests: reordering must not change the key.
	reordered := baseKey()
	reordered.LockDigests = []string{reordered.LockDigests[1], reordered.LockDigests[0]}
	if got := mustKey(t, reordered); got != base {
		t.Errorf("reordered lock digests changed the key: %s vs %s", base, got)
	}

	// Every field is load-bearing: a change to ANY field changes the key.
	mutations := map[string]func(*Key){
		"RepoKey":        func(k *Key) { k.RepoKey = "github.com/acme/other" },
		"LockDigest":     func(k *Key) { k.LockDigests[0] = "go.sum:" + strings.Repeat("ee", 32) },
		"ExtraLock":      func(k *Key) { k.LockDigests = append(k.LockDigests, "yarn.lock:"+strings.Repeat("dd", 32)) },
		"Toolchain":      func(k *Key) { k.Toolchain = "go1.25.0" },
		"Arch":           func(k *Key) { k.Arch = "amd64" },
		"SetupCmdDigest": func(k *Key) { k.SetupCmdDigest = strings.Repeat("ff", 32) },
		"ImageRef":       func(k *Key) { k.ImageRef = "drydock-sandbox:v0.6.8" },
	}
	for name, mutate := range mutations {
		k := baseKey()
		mutate(&k)
		if got := mustKey(t, k); got == base {
			t.Errorf("mutating %s did not change the key", name)
		}
	}

	// Fail closed at the key layer: no lockfile digests -> no cache key.
	noLocks := baseKey()
	noLocks.LockDigests = nil
	if _, err := ComputeKey(noLocks); err == nil {
		t.Error("ComputeKey with empty LockDigests: want error, got nil")
	}
	emptyLocks := baseKey()
	emptyLocks.LockDigests = []string{}
	if _, err := ComputeKey(emptyLocks); err == nil {
		t.Error("ComputeKey with zero-length LockDigests: want error, got nil")
	}
	noRepo := baseKey()
	noRepo.RepoKey = ""
	if _, err := ComputeKey(noRepo); err == nil {
		t.Error("ComputeKey with empty RepoKey: want error, got nil")
	}

	// Injectivity across field boundaries: content sliding from one field into
	// its neighbor must not collide. RepoKey "a" + lock "bc" vs RepoKey "ab" +
	// lock "c" concatenate identically without boundary encoding.
	left := baseKey()
	left.RepoKey, left.LockDigests = "a", []string{"bc"}
	right := baseKey()
	right.RepoKey, right.LockDigests = "ab", []string{"c"}
	if mustKey(t, left) == mustKey(t, right) {
		t.Error("RepoKey/LockDigests boundary collision: {a,[bc]} == {ab,[c]}")
	}

	// Same, between two adjacent lock-digest ELEMENTS: ["ab","c"] vs ["a","bc"].
	elemLeft := baseKey()
	elemLeft.LockDigests = []string{"ab", "c"}
	elemRight := baseKey()
	elemRight.LockDigests = []string{"a", "bc"}
	if mustKey(t, elemLeft) == mustKey(t, elemRight) {
		t.Error("lock-digest element boundary collision: [ab c] == [a bc]")
	}

	// And between adjacent scalar fields (Toolchain/Arch).
	scalarLeft := baseKey()
	scalarLeft.Toolchain, scalarLeft.Arch = "go1.2", "6arm64"
	scalarRight := baseKey()
	scalarRight.Toolchain, scalarRight.Arch = "go1.26", "arm64"
	if mustKey(t, scalarLeft) == mustKey(t, scalarRight) {
		t.Error("Toolchain/Arch boundary collision")
	}
}

func TestStore_DirCreatesAndRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}

	bad := []string{
		"",
		"..",
		"ab..",
		"../" + strings.Repeat("ab", 31) + "ab", // 64 chars containing traversal
		strings.Repeat("gg", 32),                // 64 chars, not hex
		strings.Repeat("AB", 32),                // uppercase hex rejected
		strings.Repeat("ab", 32)[:63],           // 63 hex
		strings.Repeat("ab", 32) + "a",          // 65 hex
		strings.Repeat("ab", 32) + "/x",         // trailing path component
	}
	for _, k := range bad {
		if _, err := s.Dir(k); err == nil {
			t.Errorf("Dir(%q): want error, got nil", k)
		}
	}

	key := strings.Repeat("ab", 32)
	dir, err := s.Dir(key)
	if err != nil {
		t.Fatalf("Dir(valid key): %v", err)
	}
	if want := filepath.Join(root, key); dir != want {
		t.Errorf("Dir returned %q, want %q", dir, want)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat created dir: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("%q is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 0700", perm)
	}

	// Unsafe roots are refused before any filesystem work.
	for _, badRoot := range []string{"", "/", "relative/root"} {
		bs := &Store{Root: badRoot}
		if _, err := bs.Dir(key); err == nil {
			t.Errorf("Dir with unsafe root %q: want error, got nil", badRoot)
		}
	}
}

func TestStore_EvictLRU(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root, QuotaBytes: 100}

	keys := []string{
		strings.Repeat("aa", 32),
		strings.Repeat("bb", 32),
		strings.Repeat("cc", 32),
	}
	now := time.Now()
	dirs := make([]string, len(keys))
	for i, k := range keys {
		d, err := s.Dir(k)
		if err != nil {
			t.Fatalf("Dir(%s): %v", k, err)
		}
		dirs[i] = d
		// 60 bytes of content per entry: 3 entries = 180 > quota 100.
		if err := os.WriteFile(filepath.Join(d, "blob"), make([]byte, 60), 0o600); err != nil {
			t.Fatal(err)
		}
		// Stagger access times: keys[0] least recently used.
		mt := now.Add(time.Duration(i-len(keys)) * time.Hour)
		if err := os.Chtimes(d, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	freed, err := s.Evict(0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	// 180 -> evict oldest (120 left, still over) -> evict next oldest
	// (60 left, under quota) -> stop. Two entries * 60 bytes freed.
	if freed != 120 {
		t.Errorf("freed = %d, want 120", freed)
	}
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Errorf("LRU entry %s should have been evicted", keys[0])
	}
	if _, err := os.Stat(dirs[1]); !os.IsNotExist(err) {
		t.Errorf("second-LRU entry %s should have been evicted", keys[1])
	}
	if _, err := os.Stat(dirs[2]); err != nil {
		t.Errorf("most-recent entry %s should have survived: %v", keys[2], err)
	}

	// Under quota and no free-floor pressure: nothing further evicted.
	freed, err = s.Evict(0)
	if err != nil {
		t.Fatalf("second Evict: %v", err)
	}
	if freed != 0 {
		t.Errorf("second Evict freed %d, want 0", freed)
	}
	if _, err := os.Stat(dirs[2]); err != nil {
		t.Errorf("surviving entry removed by no-op eviction: %v", err)
	}

	// Root-shape guard: unsafe roots refuse to RemoveAll anything.
	for _, badRoot := range []string{"", "/", "relative/root"} {
		bs := &Store{Root: badRoot, QuotaBytes: 1}
		if _, err := bs.Evict(0); err == nil {
			t.Errorf("Evict with unsafe root %q: want error, got nil", badRoot)
		}
	}
}

func TestLockfileDigests(t *testing.T) {
	sum := func(b []byte) string {
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}

	t.Run("two lockfiles hashed and sorted", func(t *testing.T) {
		dir := t.TempDir()
		goSum := []byte("golang.org/x/mod v0.20.0 h1:abc=\n")
		npmLock := []byte(`{"lockfileVersion": 3}`)
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), goSum, 0o600); err != nil {
			t.Fatal(err)
		}
		sub := filepath.Join(dir, "web")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "package-lock.json"), npmLock, 0o600); err != nil {
			t.Fatal(err)
		}
		// A lockfile inside .git must be ignored.
		gitDir := filepath.Join(dir, ".git")
		if err := os.Mkdir(gitDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "go.sum"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// A manifest that is not a lockfile must be ignored.
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		digests, ok := LockfileDigests(dir)
		if !ok {
			t.Fatal("hasLockfile = false, want true")
		}
		want := []string{
			"go.sum:" + sum(goSum),
			"web/package-lock.json:" + sum(npmLock),
		}
		sort.Strings(want)
		if len(digests) != len(want) {
			t.Fatalf("got %d digests %v, want %d", len(digests), digests, len(want))
		}
		for i := range want {
			if digests[i] != want[i] {
				t.Errorf("digest[%d] = %q, want %q", i, digests[i], want[i])
			}
		}
		if !sort.StringsAreSorted(digests) {
			t.Errorf("digests not sorted: %v", digests)
		}
	})

	t.Run("manifest only is not a lockfile", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		digests, ok := LockfileDigests(dir)
		if ok || len(digests) != 0 {
			t.Errorf("go.mod-only repo: got (%v, %v), want (empty, false)", digests, ok)
		}
	})

	t.Run("symlinked lockfile refused", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "not-a-lockfile.txt")
		if err := os.WriteFile(target, []byte("poison"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "go.sum")); err != nil {
			t.Fatal(err)
		}
		digests, ok := LockfileDigests(dir)
		if ok || len(digests) != 0 {
			t.Errorf("symlinked go.sum: got (%v, %v), want (empty, false)", digests, ok)
		}
	})
}
