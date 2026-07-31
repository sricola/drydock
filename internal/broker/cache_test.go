package broker

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"drydock/internal/depcache"
	"drydock/internal/trustbrief"
)

// These tests drive the dependency-cache wiring (resolve key -> rw setup
// mount -> ro agent mount -> hit/miss evidence -> in-use-safe eviction)
// through the same b.runAgent seam the setup tests use. The security
// invariants under test: the agent VM NEVER gets a writable /deps (cache
// poisoning), and Evict NEVER removes an entry a live task is using.

// depsMount returns the --mount value that targets /deps in args, or "".
func depsMount(args []string) string {
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) && strings.Contains(args[i+1], "target=/deps") {
			return args[i+1]
		}
	}
	return ""
}

// mountSource extracts the source= path from a --mount value.
func mountSource(m string) string {
	const p = "source="
	i := strings.Index(m, p)
	if i < 0 {
		return ""
	}
	rest := m[i+len(p):]
	if j := strings.Index(rest, ","); j >= 0 {
		return rest[:j]
	}
	return rest
}

// cacheBroker is testBroker plus an enabled cache (root + quota) and a
// cache-opted-in execution profile for github.com/o/r.
func cacheBroker(t *testing.T, st taskStage, grant *fakeGrant,
	run func(context.Context, []string, io.Writer, io.Writer) error) *Broker {
	t.Helper()
	b := testBroker(t, "anthropic", st, grant, run)
	b.CacheRoot = t.TempDir()
	b.CacheQuotaBytes = 1 << 30
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"npm", "ci"}}, Cache: true},
	}
	return b
}

// writeLockfile plants a package-lock.json in the stage work dir so
// LockfileDigests finds a pinned dependency set.
func writeLockfile(t *testing.T, workDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, "package-lock.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A profile with cache: true but a repo with no lockfile: caching is disabled
// for the task (no /deps mount anywhere, no cache env) and the brief records
// why. An unpinned dependency set has no stable content identity to cache
// under.
func TestCache_DisabledNoLockfile(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	grant := &fakeGrant{}
	log := &runLog{}
	b := cacheBroker(t, st, grant, setupSplitRun(log, func(context.Context, []string, io.Writer, io.Writer) error { return nil }))
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	for _, args := range log.all() {
		if m := depsMount(args); m != "" {
			t.Errorf("no lockfile -> no cache: unexpected /deps mount %q in argv %v", m, args)
		}
		if strings.Contains(strings.Join(args, " "), "npm_config_cache") {
			t.Errorf("no lockfile -> no cache env, got argv %v", args)
		}
	}
	br := readBrief(t, b, taskID(t, events))
	if br.Setup.Cache == nil {
		t.Fatal("brief setup.cache missing — the disabled reason must be recorded")
	}
	if br.Setup.Cache.Status != trustbrief.CacheDisabledNoLockfile {
		t.Errorf("cache status=%q, want %q", br.Setup.Cache.Status, trustbrief.CacheDisabledNoLockfile)
	}
	if br.Setup.Cache.Key != "" || br.Setup.Cache.Hit {
		t.Errorf("disabled cache must carry no key/hit, got %+v", br.Setup.Cache)
	}
}

// Two tasks, same repo + same lockfile digests + same profile -> the same
// computed cache key (asserted via the recorded brief keys).
func TestCache_KeyStableAcrossTasks(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	writeLockfile(t, st.workDir, `{"lockfileVersion": 3}`)
	grant := &fakeGrant{}
	log := &runLog{}
	b := cacheBroker(t, st, grant, setupSplitRun(log, func(context.Context, []string, io.Writer, io.Writer) error { return nil }))

	_, ev1, term1 := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term1["outcome"] != "pushed" {
		t.Fatalf("task 1 outcome=%v, want pushed", term1["outcome"])
	}
	_, ev2, term2 := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term2["outcome"] != "pushed" {
		t.Fatalf("task 2 outcome=%v, want pushed", term2["outcome"])
	}
	k1 := readBrief(t, b, taskID(t, ev1)).Setup.Cache
	k2 := readBrief(t, b, taskID(t, ev2)).Setup.Cache
	if k1 == nil || k2 == nil {
		t.Fatalf("cache evidence missing: %+v %+v", k1, k2)
	}
	if len(k1.Key) != 12 {
		t.Errorf("key=%q, want a 12-hex prefix", k1.Key)
	}
	if k1.Key != k2.Key {
		t.Errorf("key drifted across identical tasks: %q vs %q", k1.Key, k2.Key)
	}
}

// THE isolation test: with caching active, the setup VM's /deps mount is RW
// (no readonly flag — setup populates the cache) while the agent VM's /deps
// mount is READ-ONLY (agent-run code must never be able to poison
// dependencies consumed by other tasks). Both point at the same host entry.
func TestCache_SetupGetsRWAgentGetsRO(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	writeLockfile(t, st.workDir, `{"lockfileVersion": 3}`)
	grant := &fakeGrant{}
	log := &runLog{}
	b := cacheBroker(t, st, grant, setupSplitRun(log, func(context.Context, []string, io.Writer, io.Writer) error { return nil }))
	rec, _, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	var setupMount, agentMount string
	for _, args := range log.all() {
		name := setupContainerName(args)
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(name, "setup-"):
			setupMount = depsMount(args)
			if !strings.Contains(joined, "npm_config_cache=/deps/npm") {
				t.Errorf("setup argv missing the cache env: %v", args)
			}
		case strings.HasPrefix(name, "task-"):
			agentMount = depsMount(args)
			if !strings.Contains(joined, "npm_config_cache=/deps/npm") {
				t.Errorf("agent argv missing the cache env: %v", args)
			}
		}
	}
	if setupMount == "" {
		t.Fatal("setup VM has no /deps mount")
	}
	if strings.Contains(setupMount, "readonly") {
		t.Errorf("ISOLATION VIOLATION: setup /deps mount is read-only, cannot populate the cache: %q", setupMount)
	}
	if agentMount == "" {
		t.Fatal("agent VM has no /deps mount")
	}
	if !strings.Contains(agentMount, ",readonly") {
		t.Errorf("ISOLATION VIOLATION: agent /deps mount is WRITABLE — cache poisoning: %q", agentMount)
	}
	src := mountSource(setupMount)
	if src == "" || src != mountSource(agentMount) {
		t.Errorf("setup and agent must share one cache entry: setup=%q agent=%q", setupMount, agentMount)
	}
	if rel, err := filepath.Rel(b.CacheRoot, src); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("cache entry %q not under the cache root %q", src, b.CacheRoot)
	}
}

// First task: empty entry -> miss. Setup populates the entry (simulated by
// the setup seam writing into its own /deps source). Second task, same key:
// hit.
func TestCache_MissThenHit(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	writeLockfile(t, st.workDir, `{"lockfileVersion": 3}`)
	grant := &fakeGrant{}
	log := &runLog{}
	setupFn := func(_ context.Context, args []string, _, _ io.Writer) error {
		// The "npm ci" stand-in: write payload into the mounted cache entry.
		if dir := mountSource(depsMount(args)); dir != "" {
			if err := os.MkdirAll(filepath.Join(dir, "npm"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "npm", "blob"), []byte("tarball"), 0o644)
		}
		return nil
	}
	b := cacheBroker(t, st, grant, setupSplitRun(log, setupFn))

	_, ev1, term1 := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term1["outcome"] != "pushed" {
		t.Fatalf("task 1 outcome=%v, want pushed", term1["outcome"])
	}
	c1 := readBrief(t, b, taskID(t, ev1)).Setup.Cache
	if c1 == nil || c1.Status != trustbrief.CacheMiss || c1.Hit {
		t.Fatalf("task 1 cache=%+v, want a miss", c1)
	}

	_, ev2, term2 := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term2["outcome"] != "pushed" {
		t.Fatalf("task 2 outcome=%v, want pushed", term2["outcome"])
	}
	c2 := readBrief(t, b, taskID(t, ev2)).Setup.Cache
	if c2 == nil || c2.Status != trustbrief.CacheHit || !c2.Hit {
		t.Fatalf("task 2 cache=%+v, want a hit", c2)
	}
	if c1.Key != c2.Key {
		t.Errorf("keys differ across the miss/hit pair: %q vs %q", c1.Key, c2.Key)
	}
}

// No cache: true in the profile -> zero cache surface, even with cache_root
// configured: no /deps mount, no cache env, no cache evidence. The argvs are
// what they were before the cache existed.
func TestCache_OffByDefault(t *testing.T) {
	st := &fakeStage{workDir: t.TempDir(), diff: "diff --git a/x b/x\n+y\n"}
	writeLockfile(t, st.workDir, `{"lockfileVersion": 3}`) // lockfile present, still no cache
	grant := &fakeGrant{}
	log := &runLog{}
	b := cacheBroker(t, st, grant, setupSplitRun(log, func(context.Context, []string, io.Writer, io.Writer) error { return nil }))
	b.Setup = map[string]SetupProfile{
		"github.com/o/r": {Setup: [][]string{{"npm", "ci"}}}, // Cache deliberately unset
	}
	rec, events, term := submit(b, `{"repo_ref":"https://github.com/o/r.git","instruction":"x","agent":"claude","auto_approve":true}`)
	if term["outcome"] != "pushed" {
		t.Fatalf("outcome=%v, want pushed; body=%s", term["outcome"], rec.Body)
	}
	for _, args := range log.all() {
		if m := depsMount(args); m != "" {
			t.Errorf("cache off: unexpected /deps mount %q in argv %v", m, args)
		}
		joined := strings.Join(args, " ")
		for _, tell := range []string{"npm_config_cache", "GOMODCACHE", "PIP_CACHE_DIR", "CARGO_HOME"} {
			if strings.Contains(joined, tell) {
				t.Errorf("cache off: unexpected cache env %q in argv %v", tell, args)
			}
		}
	}
	if c := readBrief(t, b, taskID(t, events)).Setup.Cache; c != nil {
		t.Errorf("cache off: brief must carry no cache evidence, got %+v", c)
	}
	// The cache root stays untouched — no entry dir was ever created.
	entries, _ := os.ReadDir(b.CacheRoot)
	if len(entries) != 0 {
		t.Errorf("cache off: cache root must stay empty, found %d entries", len(entries))
	}
}

// THE crux: the broker's eviction sweep must never remove an entry a live
// task is using. An in-use (refcounted) entry survives a sweep even when it
// is the LRU candidate and the store is over quota; an idle entry is
// removed. Once released, the entry becomes evictable.
func TestCache_EvictSkipsInUse(t *testing.T) {
	b := &Broker{
		CacheRoot:       t.TempDir(),
		CacheQuotaBytes: 1024, // 1 KiB — two 2 KiB entries are far over quota
	}
	store := &depcache.Store{Root: b.CacheRoot, QuotaBytes: b.CacheQuotaBytes}
	keyA := strings.Repeat("a", 64)
	keyB := strings.Repeat("b", 64)
	dirA, err := store.Dir(keyA)
	if err != nil {
		t.Fatal(err)
	}
	dirB, err := store.Dir(keyB)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 2048)
	if err := os.WriteFile(filepath.Join(dirA, "blob"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "blob"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the IN-USE entry the LRU-first eviction candidate — the strongest
	// version of the test: Evict would pick it first if the refcount didn't
	// protect it.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dirA, old, old); err != nil {
		t.Fatal(err)
	}

	// Refcount the entry twice (two hypothetical tasks sharing it).
	b.cacheAcquire(dirA)
	b.cacheAcquire(dirA)

	b.SweepCache()
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("EVICTION RACE: the in-use entry was removed by a concurrent sweep: %v", err)
	}
	if _, err := os.Stat(dirB); !os.IsNotExist(err) {
		t.Error("the idle over-quota entry should have been evicted")
	}

	// One release: still in use (refcount 1), still protected.
	b.cacheRelease(dirA)
	b.SweepCache()
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("entry with refcount 1 was evicted: %v", err)
	}

	// Concurrency shakeout for -race: acquire/release/sweep from many
	// goroutines at once against the same broker.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.cacheAcquire(dirA)
			b.SweepCache()
			b.cacheRelease(dirA)
		}()
	}
	wg.Wait()

	// Final release: refcount 0 — now the sweep may remove it.
	b.cacheRelease(dirA)
	b.SweepCache()
	if _, err := os.Stat(dirA); !os.IsNotExist(err) {
		t.Error("released over-quota entry should have been evicted")
	}
}
