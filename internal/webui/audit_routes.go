package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"drydock/internal/audit"
)

type HistoryItem struct {
	ID          string `json:"id"`
	Outcome     string `json:"outcome"`
	Cost        string `json:"cost"`
	DurationMs  int64  `json:"duration_ms"`
	HasDuration bool   `json:"has_duration"`
	MtimeUnix   int64  `json:"mtime_unix"`
}

// openAuditFile opens <AuditRoot>/<id><suffix>, refusing symlinks (O_NOFOLLOW)
// and anything whose id isn't the exact task-id grammar. Returns 400/404-able
// errors via the bool: (file, 404?) — caller maps a nil file to the right code.
func (s *Server) openAuditFile(id, suffix string) (*os.File, bool) {
	if !validID(id) {
		return nil, false // caller already validated; defensive
	}
	p := filepath.Join(s.AuditRoot, id+suffix)
	f, err := os.OpenFile(p, os.O_RDONLY|syscallNoFollow, 0)
	if err != nil {
		return nil, true // treat as not-found (missing or symlink)
	}
	return f, true
}

func (s *Server) serveAuditFile(w http.ResponseWriter, r *http.Request, suffix, contentType string) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return
	}
	f, _ := s.openAuditFile(id, suffix)
	if f == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	io.Copy(w, f)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.AuditRoot)
	if err != nil {
		_ = json.NewEncoder(w).Encode([]HistoryItem{}) // empty audit dir → empty list
		return
	}
	items := []HistoryItem{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !validID(id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(s.AuditRoot, name)
		last, ok := audit.LastResult(path, info.Size())
		meta := audit.ReadMeta(path)
		items = append(items, HistoryItem{
			ID:          id,
			Outcome:     audit.Outcome(last, ok, meta),
			Cost:        audit.Cost(meta, last, ok),
			DurationMs:  last.DurationMs,
			HasDuration: audit.HasDuration(last, ok),
			MtimeUnix:   info.ModTime().Unix(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].MtimeUnix > items[j].MtimeUnix })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}
