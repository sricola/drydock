package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"drydock/internal/depcache"
	"drydock/internal/repokey"
	"drydock/internal/trustbrief"
)

// The dependency-cache wiring. THE serialization invariant, on which the
// cache's safety rests: depcache.Store.Evict scans the cache root and can
// RemoveAll ANY entry, so eviction must be EXCLUSIVE against all cache use.
// b.cacheMu guards both sides — resolveCache (key + Dir + refcount++) and
// sweepCache (Evict) each hold it for their whole critical section — and the
// b.cacheInUse refcount map keeps a sweep from ever removing an entry a live
// task has mounted: resolve increments before the setup VM boots, the
// release (deferred in HandleTask, so it covers BOTH the setup and the agent
// run) decrements at task completion. Sweeps run only at brokerd boot
// (cmd/brokerd calls SweepCache before any task exists) and at
// task-completion boundaries (releaseCache) — never between a task's
// resolve and its completion.

// cacheStoreLocked returns the store, building it lazily from
// CacheRoot/CacheQuotaBytes. nil = cache off (no root, or zero quota).
// Callers MUST hold b.cacheMu.
func (b *Broker) cacheStoreLocked() *depcache.Store {
	if b.CacheRoot == "" || b.CacheQuotaBytes <= 0 {
		return nil
	}
	if b.cacheStore == nil {
		b.cacheStore = &depcache.Store{Root: b.CacheRoot, QuotaBytes: b.CacheQuotaBytes}
	}
	return b.cacheStore
}

// cacheAcquire/cacheRelease maintain the in-use refcount for one entry dir.
func (b *Broker) cacheAcquire(dir string) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cacheAcquireLocked(dir)
}

// cacheAcquireLocked is cacheAcquire for callers already holding b.cacheMu
// (resolveCache increments inside its own critical section).
func (b *Broker) cacheAcquireLocked(dir string) {
	if b.cacheInUse == nil {
		b.cacheInUse = make(map[string]int)
	}
	b.cacheInUse[dir]++
}

func (b *Broker) cacheRelease(dir string) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if n := b.cacheInUse[dir]; n <= 1 {
		delete(b.cacheInUse, dir)
	} else {
		b.cacheInUse[dir] = n - 1
	}
}

// SweepCache runs one eviction pass over the dependency cache, holding the
// cache mutex for the whole scan-and-remove and excluding every in-use
// entry, so it can never delete a directory a live VM has bind-mounted.
// No-op when the cache is off. Called by cmd/brokerd once at boot (before
// any task can be running) and by releaseCache after each cache-using task
// completes.
func (b *Broker) SweepCache() {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	store := b.cacheStoreLocked()
	if store == nil {
		return
	}
	protected := make(map[string]bool, len(b.cacheInUse))
	for d := range b.cacheInUse {
		protected[d] = true
	}
	freed, err := store.EvictExcluding(minFreeStageBytes, protected)
	if err != nil {
		slog.Warn("depcache: eviction sweep failed", "err", err)
	}
	if freed > 0 {
		slog.Info("depcache: evicted least-recently-used entries", "freed_mib", freed>>20)
	}
}

// resolveCache decides this task's cache participation and, when active,
// resolves (and refcounts) its entry dir. Called by runSetup before the
// first setup VM boots. It sets tr.cacheDir/cacheKey/cacheHit/cacheStatus;
// cacheDir == "" means the task runs uncached (either off entirely —
// status "" — or disabled for cause, status recorded for the Brief).
//
// The whole resolve happens under b.cacheMu: the key computation reads
// host-side lockfiles (bounded walk), Dir creates/stamps the entry, and the
// refcount increment must be atomic with the resolve so no sweep can
// interleave between "dir chosen" and "dir protected".
func (tr *taskRun) resolveCache(cfg SetupProfile) {
	b := tr.b
	if !cfg.Cache {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	store := b.cacheStoreLocked()
	if store == nil {
		return
	}
	digests, ok := depcache.LockfileDigests(tr.st.WorkDir())
	if !ok {
		// Opted in, but nothing pins the dependency set: no cache (an
		// unpinned tree has no stable content identity), reason recorded.
		tr.cacheStatus = trustbrief.CacheDisabledNoLockfile
		return
	}
	key, err := depcache.ComputeKey(depcache.Key{
		RepoKey:     repokey.Normalize(tr.repoRef),
		LockDigests: digests,
		// Toolchain is carried by ImageRef in this deployment: the sandbox
		// image pins every tool version setup can run. A finer-grained
		// marker can be added to the Key without a migration (old entries
		// simply miss).
		Toolchain:      "",
		Arch:           runtime.GOARCH,
		SetupCmdDigest: setupCmdDigest(cfg),
		ImageRef:       b.ImageRef,
	})
	if err != nil {
		// ComputeKey fails closed on empty repokey/digests — neither can
		// happen here, but if it does the task just runs uncached.
		slog.Warn("depcache: key computation failed; running uncached", "task_id", tr.id, "err", err)
		return
	}
	dir, err := store.Dir(key)
	if err != nil {
		slog.Warn("depcache: entry dir unavailable; running uncached", "task_id", tr.id, "err", err)
		return
	}
	// A hit = the entry already carried payload before this run (Dir creates
	// an empty dir on first use, so non-empty means an earlier setup
	// populated it).
	entries, _ := os.ReadDir(dir)
	tr.cacheDir = dir
	tr.cacheKey = key
	tr.cacheHit = len(entries) > 0
	if tr.cacheHit {
		tr.cacheStatus = trustbrief.CacheHit
	} else {
		tr.cacheStatus = trustbrief.CacheMiss
	}
	b.cacheAcquireLocked(dir)
}

// releaseCache ends this task's cache use: it drops the entry's refcount and
// runs a post-completion eviction sweep (which skips anything still in use
// by other tasks). Deferred in HandleTask BEFORE runSetup so the refcount
// held by resolveCache spans the setup VMs AND the agent run — the entry
// must not be evictable while either has it bind-mounted. No-op when the
// task ran uncached.
func (tr *taskRun) releaseCache() {
	if tr.cacheDir == "" {
		return
	}
	tr.b.cacheRelease(tr.cacheDir)
	tr.b.SweepCache()
}

// cacheEvidence renders the task's cache participation for the Brief's
// setup block: nil when caching was off entirely, a status-only record when
// disabled for cause, and key-prefix+hit when active. Broker-observed.
func (tr *taskRun) cacheEvidence() *trustbrief.SetupCache {
	switch tr.cacheStatus {
	case "":
		return nil
	case trustbrief.CacheDisabledNoLockfile:
		return &trustbrief.SetupCache{Status: tr.cacheStatus}
	default:
		return &trustbrief.SetupCache{Status: tr.cacheStatus, Key: tr.cacheKey[:12], Hit: tr.cacheHit}
	}
}

// setupCmdDigest hashes the profile's setup+readiness argvs into the cache
// key's SetupCmdDigest component, so a changed setup command can never reuse
// an entry produced by the old one. Same injective-encoding idiom as
// depcache.ComputeKey (and config's profilesHash): every token is
// strconv.Quote'd — quoting escapes quotes and separators, so no argv can
// forge a token, argv, or section boundary.
func setupCmdDigest(cfg SetupProfile) string {
	var sb strings.Builder
	for _, argv := range cfg.Setup {
		sb.WriteString("setup=")
		for _, a := range argv {
			sb.WriteString(strconv.Quote(a))
			sb.WriteByte(' ')
		}
		sb.WriteByte('\n')
	}
	for _, argv := range cfg.Readiness {
		sb.WriteString("readiness=")
		for _, a := range argv {
			sb.WriteString(strconv.Quote(a))
			sb.WriteByte(' ')
		}
		sb.WriteByte('\n')
	}
	h := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(h[:])
}
