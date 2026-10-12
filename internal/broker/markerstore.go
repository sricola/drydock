package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"drydock/internal/atomicfile"
)

// markerStore is the one implementation behind the three durable per-task
// marker files brokerd resumes from after a restart: <id>.queue.json (the
// queue item), <id>.ci.json (a CI watch), and <id>.gate.json (a task parked
// at the approval gate). Each used to carry its own copy of read/write/list/
// remove with slightly different hardening; a marker type is now correct by
// construction:
//
//   - the id is validated against newID's shape BEFORE any path is built, so
//     a hostile id can never traverse out of the audit root;
//   - writes are atomic and durable (atomicfile: per-process temp, fsync,
//     rename) and 0600, because markers carry repo refs and instructions;
//   - reads refuse a symlink (O_NOFOLLOW), so a planted <id>.<suffix> ->
//     elsewhere cannot feed the resume path substituted bytes;
//   - remove is idempotent and sweeps the temps a crashed write left;
//   - list skips an unreadable or malformed file with a warning naming it,
//     never failing the whole boot scan for one bad marker.
//
// The per-type files keep their own ordering and body checks on top.
type markerStore[T any] struct {
	suffix string // ".queue.json"
	what   string // "queue item", for errors and log lines
}

// path validates id and returns the marker's path under root.
func (s markerStore[T]) path(root, id string) (string, error) {
	if !queueIDRE.MatchString(id) {
		return "", fmt.Errorf("%s: invalid task id %q", s.what, id)
	}
	return filepath.Join(root, id+s.suffix), nil
}

func (s markerStore[T]) write(root, id string, v T) error {
	p, err := s.path(root, id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(p, payload, 0o600)
}

func (s markerStore[T]) read(root, id string) (T, error) {
	var v T
	p, err := s.path(root, id)
	if err != nil {
		return v, err
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return v, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(data, &v)
	return v, err
}

func (s markerStore[T]) remove(root, id string) error {
	p, err := s.path(root, id)
	if err != nil {
		return err
	}
	if err := atomicfile.RemoveStaleTemps(p); err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// list returns every readable marker under root keyed by the id its file
// name carries. A missing root is an empty listing; any other ReadDir error
// is returned. Entries with the wrong suffix, directories, and markers that
// fail to open (symlinks included) or parse are skipped with a warning.
func (s markerStore[T]) list(root string) (map[string]T, error) {
	out := map[string]T{}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), s.suffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), s.suffix)
		v, err := s.read(root, id)
		if err != nil {
			slog.Warn(s.what+": skipping unreadable marker", "file", e.Name(), "err", err)
			continue
		}
		out[id] = v
	}
	return out, nil
}
