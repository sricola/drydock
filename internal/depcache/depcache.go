// Package depcache is the host-side dependency cache: a content-addressed
// store of per-repo dependency trees keyed by a hash of everything that
// determines their content. The KEY is the poisoning defense — cache entries
// are shared only between runs whose repo identity, lockfile contents,
// toolchain, architecture, setup command, and sandbox image ALL agree, so a
// hostile repo cannot craft inputs that collide into another repo's entry and
// no entry outlives a change to any input that produced it. That is why
// ComputeKey's encoding must be injective (no field-boundary collisions) and
// complete (every trust-relevant input included), and why "no lockfile" is an
// error rather than an empty component: an unpinned dependency set has no
// stable content identity to cache under.
package depcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"drydock/internal/trustbrief"
)

// Key is every input that determines a dependency tree's content. Adding a
// field here is safe (old entries simply miss); removing one risks sharing
// entries across runs that should not share.
type Key struct {
	// RepoKey is the canonical repo identity (internal/repokey) — isolates
	// repos from each other.
	RepoKey string
	// LockDigests are the "path:sha256" strings from LockfileDigests — pin
	// the exact dependency set.
	LockDigests []string
	// Toolchain and Arch pin the build environment (a Node 20 node_modules
	// tree is not a Node 22 one; arm64 native modules are not amd64 ones).
	Toolchain string
	Arch      string
	// SetupCmdDigest pins the setup command that produced the tree.
	SetupCmdDigest string
	// ImageRef pins the sandbox image the setup ran in.
	ImageRef string
}

// ComputeKey returns the cache-entry name for k: sha256 (full 64 hex — it is
// a directory name, not a display string) over a canonical, injective
// encoding. Mirrors internal/config's profilesHash/globsHash idiom:
// LockDigests are sorted (a copy — the caller's slice is never mutated) so
// element order is irrelevant, and every field value and list element is
// strconv.Quote'd under a fixed "label=" prefix, one field per line. Quoting
// escapes the quote and newline characters themselves, so no value can forge
// a field boundary — {RepoKey:"a", locks:["bc"]} and {RepoKey:"ab",
// locks:["c"]} encode differently, as do ["ab","c"] and ["a","bc"].
//
// Fails closed: an empty RepoKey or an empty LockDigests is an error — no
// lockfile means no cache, enforced here at the key layer so no caller can
// forget the rule.
func ComputeKey(k Key) (string, error) {
	if k.RepoKey == "" {
		return "", errors.New("depcache: empty RepoKey")
	}
	if len(k.LockDigests) == 0 {
		return "", errors.New("depcache: no lockfile digests (no lockfile, no cache)")
	}
	locks := append([]string(nil), k.LockDigests...)
	sort.Strings(locks)
	quoted := make([]string, len(locks))
	for i, d := range locks {
		quoted[i] = strconv.Quote(d)
	}
	var b strings.Builder
	b.WriteString("repokey=")
	b.WriteString(strconv.Quote(k.RepoKey))
	b.WriteString("\nlocks=")
	b.WriteString(strings.Join(quoted, " "))
	b.WriteString("\ntoolchain=")
	b.WriteString(strconv.Quote(k.Toolchain))
	b.WriteString("\narch=")
	b.WriteString(strconv.Quote(k.Arch))
	b.WriteString("\nsetupcmd=")
	b.WriteString(strconv.Quote(k.SetupCmdDigest))
	b.WriteString("\nimage=")
	b.WriteString(strconv.Quote(k.ImageRef))
	b.WriteByte('\n')
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:]), nil
}

// Store is the on-disk cache: one directory per key under Root. QuotaBytes
// bounds the total content bytes across entries; <= 0 means no byte quota
// (the host free-space floor passed to Evict still applies).
type Store struct {
	Root       string
	QuotaBytes int64
}

// keyPattern is exactly what ComputeKey emits. Anything else — traversal
// dots, separators, uppercase, wrong length — is rejected before it can
// become a path component.
var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// safeRoot validates Root's shape (mirrors internal/stage Cleanup's guard):
// Evict RemoveAlls entry dirs under it, so an empty/"/"/relative Root must be
// refused before any filesystem work.
func (s *Store) safeRoot() (string, error) {
	clean := filepath.Clean(s.Root)
	if clean == "" || clean == "/" || clean == "." || !filepath.IsAbs(clean) {
		return "", fmt.Errorf("depcache: refusing unsafe cache root %q", s.Root)
	}
	return clean, nil
}

// Dir returns the entry directory for key, creating it 0700 on first use and
// stamping the directory's times to now — the access marker Evict's LRU
// ordering reads. Rejects any key that is not exactly 64 lowercase hex.
func (s *Store) Dir(key string) (string, error) {
	root, err := s.safeRoot()
	if err != nil {
		return "", err
	}
	if !keyPattern.MatchString(key) {
		return "", fmt.Errorf("depcache: invalid cache key %q", key)
	}
	dir := filepath.Join(root, key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		return "", err
	}
	return dir, nil
}

// Evict removes least-recently-accessed entries (by the Dir-stamped entry-dir
// mtime) until total cache bytes are within QuotaBytes AND host free space at
// Root is at least minFreeBytes (a floor <= 0 disables the free-space check).
// Only direct children whose names match keyPattern are considered or
// removed. Returns the content bytes freed.
func (s *Store) Evict(minFreeBytes int64) (freed int64, err error) {
	root, err := s.safeRoot()
	if err != nil {
		return 0, err
	}
	dirents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // no cache yet — nothing to evict
		}
		return 0, err
	}
	type entry struct {
		path  string
		mtime time.Time
		bytes int64
	}
	var entries []entry
	var total int64
	for _, de := range dirents {
		if !de.IsDir() || !keyPattern.MatchString(de.Name()) {
			continue
		}
		info, ierr := de.Info()
		if ierr != nil {
			continue // vanished mid-scan; best effort
		}
		p := filepath.Join(root, de.Name())
		n := dirBytes(p)
		entries = append(entries, entry{path: p, mtime: info.ModTime(), bytes: n})
		total += n
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })

	overQuota := func() bool { return s.QuotaBytes > 0 && total > s.QuotaBytes }
	belowFloor := func() bool {
		if minFreeBytes <= 0 {
			return false
		}
		free, ferr := hostFree(root)
		return ferr == nil && free < minFreeBytes
	}
	for _, e := range entries {
		if !overQuota() && !belowFloor() {
			break
		}
		if rerr := os.RemoveAll(e.path); rerr != nil {
			return freed, rerr
		}
		total -= e.bytes
		freed += e.bytes
	}
	return freed, nil
}

// dirBytes sums regular-file sizes under dir, best effort (a vanished file
// mid-walk just isn't counted).
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// hostFree returns the bytes available to an unprivileged user on the
// filesystem containing path (replicates internal/broker's freeBytes — that
// helper is unexported and package-local by design).
func hostFree(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Walk bounds for LockfileDigests: workDir is an untrusted checkout, so the
// walk is capped like internal/broker's stage walkers — a pathological tree
// (fork-bomb directory fanout, millions of files) terminates the scan instead
// of hanging the broker.
const (
	maxWalkFiles = 100_000
	maxWalkDepth = 16
)

// LockfileDigests walks workDir for known lockfile base names
// (trustbrief.LockfileNames — the same set the Trust Brief flags) and returns
// their sorted "relpath:sha256hex" digests plus whether any were found. The
// caller disables caching entirely when hasLockfile is false.
//
// Security properties: .git is skipped; each candidate is opened
// O_RDONLY|O_NOFOLLOW and additionally type-checked, so a symlinked
// "lockfile" is refused (not hashed, not counted) — a symlink pointing at
// attacker-chosen content must not be able to stamp a cache identity. The
// slash-normalized repo-relative path in each digest string keeps two
// same-named lockfiles in different directories distinct, and each digest
// string is itself injective (the sha256 hex is always the fixed-length
// suffix after the last colon).
func LockfileDigests(workDir string) ([]string, bool) {
	known := make(map[string]bool)
	for _, n := range trustbrief.LockfileNames() {
		known[n] = true
	}
	var digests []string
	files := 0
	_ = filepath.WalkDir(workDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, best effort
		}
		if p == workDir {
			return nil
		}
		rel, rerr := filepath.Rel(workDir, p)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if strings.Count(rel, string(filepath.Separator))+1 > maxWalkDepth {
				return filepath.SkipDir
			}
			return nil
		}
		files++
		if files > maxWalkFiles {
			return filepath.SkipAll
		}
		if !known[d.Name()] {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // symlinked lockfile: refused
		}
		sum, herr := fileSHA256NoFollow(p)
		if herr != nil {
			return nil // O_NOFOLLOW refusal or read error: not hashed
		}
		digests = append(digests, filepath.ToSlash(rel)+":"+sum)
		return nil
	})
	sort.Strings(digests)
	return digests, len(digests) > 0
}

// fileSHA256NoFollow hashes a file's bytes, refusing symlinks (replicates
// internal/broker's helper of the same name).
func fileSHA256NoFollow(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
