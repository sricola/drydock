package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"drydock/internal/atomicfile"
)

// CIState is the broker's LAST OBSERVATION of a pushed PR's CI. It is
// broker-observed evidence, never anything the task's VM or the repo's own
// workflow logs asserted (D3): only check conclusions read by the host may
// move it. The zero value is deliberately not a member — an unwritten state is
// "nothing observed yet", which CIPending names explicitly.
type CIState string

const (
	// CIPending: the watch is live and no terminal conclusion has been seen.
	// This is what finishPush writes: a marker exists, nothing is known yet.
	CIPending CIState = "pending"
	// CIPassed: every observed check concluded successfully.
	CIPassed CIState = "passed"
	// CIFailed: at least one observed check concluded in failure.
	CIFailed CIState = "failed"
	// CINoChecks: the PR has no checks configured. Distinct from CIPassed —
	// absence of evidence never reads as passing (the verifier's rule).
	CINoChecks CIState = "no_checks"
)

// ciMarker is the durable record that a pushed PR is under host-side CI
// observation. It is the watch's only cross-restart state: the watcher holds no
// concurrency slot and keeps no stage (D4), so everything it needs to resume
// after a brokerd restart must live here.
//
// Written by finishPush only when the push SUCCEEDED, the adapter reported a PR
// identity, and the watch is enabled. Its absence is therefore meaningful —
// "this task has no watchable PR" — and is never silently read as success (D7).
//
// Attempt/RetryOf carry the bounded-retry chain (D6) so the bound survives a
// restart and cannot be laundered by a crash. B1 always writes them zero; they
// exist now so B2 needs no schema change to an on-disk format that will already
// have live markers written by a B1 daemon.
type ciMarker struct {
	TaskID string `json:"task_id"`
	// RepoRef is the TASK's repository — the one it cloned and pushed to — with
	// any embedded clone credential redacted (trustbrief.RedactRepoRef), same
	// as the Brief: this is a durable artifact, and the userinfo in
	// https://user:tok@host/... must not land in one. It is kept for display
	// and for ONE check: its host. It is NOT the coordinate the watch queries
	// (see PROwner/PRRepo) and it is not a clone source — a B2 retry re-clones
	// from the persisted queue item's Task, never from this field.
	RepoRef  string `json:"repo_ref"`
	Branch   string `json:"branch"`
	PRNumber int    `json:"pr_number"`
	// PROwner/PRRepo are the repository THE PULL REQUEST ITSELF lives in, and
	// they are AUTHORITATIVE for the watch: that is where the checks are.
	//
	// They are captured and validated at PR-open time (remote.parsePRURL:
	// host-pinned to the task's ref, four exact path segments, each of
	// owner/repo through validateOwnerRepo) and stored as explicit fields so
	// the watcher reads validated data and NEVER re-parses PRURL.
	//
	// They may legitimately differ from RepoRef: `gh pr create` run in a fork
	// opens the PR against the UPSTREAM repository. The capture therefore pins
	// only the HOST (a PR must live on the same host as the task's repo);
	// owner/repo are free to differ, and the PR's own pair wins.
	PROwner string `json:"pr_owner"`
	PRRepo  string `json:"pr_repo"`
	// PRURL is the canonical URL, rebuilt from the validated pieces above. It
	// is for display and operator navigation ONLY — nothing may re-parse it to
	// recover a coordinate.
	PRURL string `json:"pr_url"`
	// Attempt is 1 for a first attempt, N for the Nth retry in a chain.
	// RetryOf is the parent task id ("" for a first attempt).
	Attempt int    `json:"attempt,omitempty"`
	RetryOf string `json:"retry_of,omitempty"`
	// State is the last observation. See CIState.
	State CIState `json:"state"`
	// Timestamps are unix-millis and are ALWAYS caller-supplied — nothing in
	// this file calls time.Now, matching queuestore, so tests are
	// deterministic and the broker's clock seam stays the single source.
	// DeadlineMs 0 means "no watch deadline recorded" (the timeout is wired in
	// a later task); a watcher must treat 0 as unbounded-by-this-field and
	// apply its own configured bound.
	//
	// FloorMs is the same idea for the DISPATCH FLOOR (ciwatch.go): the
	// materialized absolute instant before which a non-failing rollup is too
	// partial to believe. 0 means "not materialized yet"; the watcher then
	// computes it from CreatedAtMs and its configured poll interval and writes
	// it back on the first refresh. Persisting it is what makes the floor's
	// LENGTH absolute and not just its anchor — an operator who changes
	// ci.poll_interval between boots must not be able to move the floor of a
	// watch that is already running, exactly as they cannot move its deadline.
	CreatedAtMs int64 `json:"created_at_ms"`
	UpdatedAtMs int64 `json:"updated_at_ms"`
	DeadlineMs  int64 `json:"deadline_ms,omitempty"`
	FloorMs     int64 `json:"floor_ms,omitempty"`
}

const ciSuffix = ".ci.json"

// ciMarkerPath builds the marker path. Callers MUST have validated id against
// queueIDRE first — every entry point below does, before any path is built.
func ciMarkerPath(auditRoot, id string) string {
	return filepath.Join(auditRoot, id+ciSuffix)
}

// writeCIMarker durably persists m under auditRoot. Same idiom as
// writeQueueItem: id validated before a path exists (traversal guard),
// atomicfile.Write (temp + rename) so a crash mid-write can never leave a
// truncated marker where a whole one is expected, and 0600 because the marker
// carries the repo ref and PR URL.
func writeCIMarker(auditRoot string, m ciMarker) error {
	if !queueIDRE.MatchString(m.TaskID) {
		return fmt.Errorf("cimarker: invalid task id %q", m.TaskID)
	}
	if err := os.MkdirAll(auditRoot, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(ciMarkerPath(auditRoot, m.TaskID), payload, 0o600)
}

// readCIMarker loads one marker by id. The id shape is validated BEFORE the
// path is built, and the open refuses symlinks (O_NOFOLLOW, parity with the
// write side) so a planted <id>.ci.json -> elsewhere cannot feed the watcher
// substituted PR coordinates — which would aim the host's gh credential at an
// attacker-chosen repo.
func readCIMarker(auditRoot, id string) (ciMarker, error) {
	var m ciMarker
	if !queueIDRE.MatchString(id) {
		return m, fmt.Errorf("cimarker: invalid task id %q", id)
	}
	f, err := os.OpenFile(ciMarkerPath(auditRoot, id), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return m, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

// removeCIMarker deletes the marker (and any stray .tmp a crashed atomic write
// left behind). Idempotent: a missing file is success, so a remove raced
// against a restart's boot scan cannot fail the caller.
func removeCIMarker(auditRoot, id string) error {
	if !queueIDRE.MatchString(id) {
		return fmt.Errorf("cimarker: invalid task id %q", id)
	}
	path := ciMarkerPath(auditRoot, id)
	if err := os.Remove(path + ".tmp"); err != nil && !os.IsNotExist(err) {
		return err
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// listCIMarkers is the boot scan: every parseable *.ci.json in auditRoot,
// oldest-first by CreatedAtMs. A garbage, unreadable, or symlinked marker is
// skipped with a warning rather than failing the scan — one corrupt marker must
// not strand every other pending watch. A missing audit dir is not an error.
//
// Unexported, like listQueueItems: it returns the unexported ciMarker, so an
// exported form would be unusable from outside the package anyway.
func listCIMarkers(auditRoot string) ([]ciMarker, error) {
	entries, err := os.ReadDir(auditRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ciMarker
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ciSuffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ciSuffix)
		m, err := readCIMarker(auditRoot, id)
		if err != nil {
			slog.Warn("cimarker: skipping unreadable ci marker", "file", e.Name(), "err", err)
			continue
		}
		// The body must name the file it was found in. Every consumer keys off
		// m.TaskID — concludeCIWatch removes the marker by removeCIMarker(
		// m.TaskID) — so a marker whose body names a DIFFERENT id would delete
		// some other task's path, leak the file it was actually read from
		// (which is then re-concluded on every subsequent pass), and attribute
		// its observation to an unrelated task. The file name is the identity;
		// the body has to agree with it.
		if m.TaskID != id {
			slog.Warn("cimarker: skipping ci marker whose body names a different task id",
				"file", e.Name(), "body_task_id", m.TaskID)
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAtMs < out[j].CreatedAtMs })
	return out, nil
}
