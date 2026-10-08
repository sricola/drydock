package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeQueueCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQueueMaxDenials_DefaultIsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 2 {
		t.Fatalf("default queue.max_denials=%d, want 2", c.Queue.MaxDenials)
	}
}

func TestQueueMaxDenials_YAMLAndEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := writeQueueCfg(t, "queue:\n  max_denials: 4\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 4 {
		t.Fatalf("yaml queue.max_denials=%d, want 4", c.Queue.MaxDenials)
	}
	t.Setenv("DRYDOCK_QUEUE_MAX_DENIALS", "0")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 0 {
		t.Fatalf("env override not applied: %d", c.Queue.MaxDenials)
	}
	t.Setenv("DRYDOCK_QUEUE_MAX_DENIALS", "-3")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxDenials != 4 {
		t.Fatalf("negative env must be ignored, got %d", c.Queue.MaxDenials)
	}
}

func TestQueueMaxDenials_ValidateBounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, bad := range []string{"-1", "11"} {
		_, err := Load(writeQueueCfg(t, "queue:\n  max_denials: "+bad+"\n"))
		if err == nil || !strings.Contains(err.Error(), "queue.max_denials") {
			t.Errorf("max_denials=%s: want a queue.max_denials validation error, got %v", bad, err)
		}
	}
	if _, err := Load(writeQueueCfg(t, "queue:\n  max_denials: 10\n")); err != nil {
		t.Errorf("max_denials=10 must be accepted: %v", err)
	}
}

func TestQueueMaxDenials_ExplainRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DRYDOCK_QUEUE_MAX_DENIALS", "5")
	fields, _, err := Explain(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if f.YAMLKey == "queue.max_denials" {
			if f.Value != "5" || f.Source != SourceEnv || f.EnvVar != "DRYDOCK_QUEUE_MAX_DENIALS" {
				t.Fatalf("explain row: %+v", f)
			}
			return
		}
	}
	t.Fatal("policy explain has no queue.max_denials row")
}
