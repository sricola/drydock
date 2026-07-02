package broker

import (
	"testing"
)

func TestDiffStat(t *testing.T) {
	diff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-old\n+new\n+more\n"
	files, ins, del := diffStat(diff)
	if files != 1 || ins != 2 || del != 1 {
		t.Errorf("diffStat = (%d,%d,%d), want (1,2,1)", files, ins, del)
	}
}
