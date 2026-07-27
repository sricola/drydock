package broker

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiffStat(t *testing.T) {
	diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-old\n+new\n+more\n"
	files, ins, del := diffStat(diff)
	if files != 1 || ins != 2 || del != 1 {
		t.Errorf("diffStat = (%d,%d,%d), want (1,2,1)", files, ins, del)
	}
}

func TestEmit_StampsTS(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newStream(rec)
	s.emit(map[string]any{"event": "stage", "stage": "preparing"})
	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &ev); err != nil {
		t.Fatal(err)
	}
	tsStr, _ := ev["ts"].(string)
	if _, err := time.Parse(time.RFC3339, tsStr); err != nil {
		t.Fatalf("ts=%q not RFC3339: %v", tsStr, err)
	}
}
