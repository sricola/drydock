package trustbrief

import (
	"strings"
	"testing"
)

// bigDiff builds a realistic unified diff of roughly n hunk lines across a few
// files, so the benchmark exercises the per-line hot path (content lines
// dominate) plus the header/mode classification.
func bigDiff(nLines int) string {
	var b strings.Builder
	b.WriteString("diff --git a/pkg/main.go b/pkg/main.go\n")
	b.WriteString("index 1111111..2222222 100644\n")
	b.WriteString("--- a/pkg/main.go\n+++ b/pkg/main.go\n")
	b.WriteString("@@ -1,4 +1,400004 @@ func main() {\n")
	for i := 0; i < nLines; i++ {
		if i%3 == 0 {
			b.WriteString("+\tadded line of source code that is reasonably long\n")
		} else if i%3 == 1 {
			b.WriteString("-\tremoved line of source code that is reasonably long\n")
		} else {
			b.WriteString(" \tcontext line of source code that is reasonably long\n")
		}
	}
	return b.String()
}

func BenchmarkAnalyze(b *testing.B) {
	diff := bigDiff(300000) // ~15 MiB
	b.SetBytes(int64(len(diff)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Analyze(diff)
	}
}
