package main

import (
	"strings"
	"testing"
)

func TestConsume_PipedHappyPath(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"accepted","task_id":"7f3a0000","repo":"r"}`,
		`{"event":"stage","stage":"preparing"}`,
		`{"event":"stage","stage":"running","agent":"claude"}`,
		`{"event":"stage","stage":"pushing","branch":"agent/7f3a0000"}`,
		`{"event":"result","outcome":"pushed","branch":"agent/7f3a0000","platform":"github","files":4,"insertions":120,"deletions":8,"duration_ms":138000,"cost_usd":0.11}`,
	}, "\n") + "\n"

	var out strings.Builder
	exit := consume(strings.NewReader(stream), &out, modePiped)
	got := out.String()

	if exit != 0 {
		t.Errorf("exit=%d, want 0", exit)
	}
	for _, want := range []string{"accepted", "preparing", "running", "pushing", "pushed", "agent/7f3a0000", "github", "4 files", "+120", "-8", "$0.11"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestConsume_ApprovalBlock(t *testing.T) {
	stream := `{"event":"accepted","task_id":"7f3a0000"}` + "\n" +
		`{"event":"stage","stage":"awaiting_approval","diff_bytes":1234,"files":4,"approve":"drydock approve 7f3a0000","deny":"drydock deny 7f3a0000","review":"drydock review 7f3a0000"}` + "\n" +
		`{"event":"result","outcome":"pushed","branch":"b","platform":"github"}` + "\n"

	var out strings.Builder
	consume(strings.NewReader(stream), &out, modePiped)
	got := out.String()
	for _, want := range []string{"awaiting approval", "drydock approve 7f3a0000", "drydock review 7f3a0000"} {
		if !strings.Contains(got, want) {
			t.Errorf("approval block missing %q:\n%s", want, got)
		}
	}
}

func TestConsume_ErrorExitsOne(t *testing.T) {
	stream := `{"event":"accepted","task_id":"7f3a0000"}` + "\n" +
		`{"event":"error","reason":"entrypoint.sh: missing gateway ip","hint":"run drydock doctor","audit":"/x.jsonl"}` + "\n"

	var out strings.Builder
	exit := consume(strings.NewReader(stream), &out, modePiped)
	if exit != 1 {
		t.Errorf("exit=%d, want 1", exit)
	}
	got := out.String()
	for _, want := range []string{"missing gateway ip", "drydock doctor"} {
		if !strings.Contains(got, want) {
			t.Errorf("error output missing %q:\n%s", want, got)
		}
	}
}

func TestConsume_LegacyObjectFallsBackToPrintPretty(t *testing.T) {
	stream := `{"task_id":"7f3a0000","branch":"agent/7f3a0000","platform":"github","pushed":true}` + "\n"
	var out strings.Builder
	exit := consume(strings.NewReader(stream), &out, modePiped)
	if exit != 0 {
		t.Errorf("exit=%d, want 0", exit)
	}
	if !strings.Contains(out.String(), "pushed agent/7f3a0000") {
		t.Errorf("legacy fallback output unexpected:\n%s", out.String())
	}
}

func TestConsume_QuietPrintsOnlyResult(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"accepted","task_id":"7f3a0000"}`,
		`{"event":"stage","stage":"running","agent":"claude"}`,
		`{"event":"result","outcome":"pushed","branch":"b","platform":"github"}`,
	}, "\n") + "\n"
	var out strings.Builder
	consume(strings.NewReader(stream), &out, modeQuiet)
	got := out.String()
	if strings.Contains(got, "running") || strings.Contains(got, "accepted") {
		t.Errorf("quiet mode leaked progress:\n%s", got)
	}
	if !strings.Contains(got, "pushed") {
		t.Errorf("quiet mode dropped the result:\n%s", got)
	}
}

func TestConsume_PrematureEOF(t *testing.T) {
	// Stream ends with no terminal result/error event — brokerd died mid-task.
	stream := `{"event":"accepted","task_id":"7f3a0000"}` + "\n" +
		`{"event":"stage","stage":"running","agent":"claude"}` + "\n"
	var out strings.Builder
	exit := consume(strings.NewReader(stream), &out, modePiped)
	if exit != 1 {
		t.Errorf("exit=%d, want 1", exit)
	}
	if !strings.Contains(out.String(), "connection closed") {
		t.Errorf("expected premature-EOF message, got:\n%s", out.String())
	}
}

func TestConsume_NonPushOutcomes(t *testing.T) {
	cases := map[string]string{
		`{"event":"result","outcome":"no_diff"}`:   "no changes",
		`{"event":"result","outcome":"denied"}`:    "denied",
		`{"event":"result","outcome":"cancelled"}`: "cancelled",
	}
	for line, want := range cases {
		stream := `{"event":"accepted","task_id":"7f3a0000"}` + "\n" + line + "\n"
		var out strings.Builder
		exit := consume(strings.NewReader(stream), &out, modePiped)
		if exit != 0 {
			t.Errorf("%s: exit=%d, want 0", line, exit)
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("%s: output %q missing %q", line, out.String(), want)
		}
	}
}
