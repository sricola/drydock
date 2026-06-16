package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindImageDir_RespectsEnvOverride(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DRYDOCK_IMAGE_DIR", root)
	got, err := findImageDir("image")
	if err != nil {
		t.Fatalf("findImageDir: %v", err)
	}
	if got != imageDir {
		t.Errorf("got %q, want %q", got, imageDir)
	}
}

func TestFindImageDir_NotFoundReportsCandidates(t *testing.T) {
	// Use a unique path that no candidate could match.
	t.Setenv("DRYDOCK_IMAGE_DIR", "/this/path/does/not/exist")
	t.Setenv("HOMEBREW_PREFIX", "/another/missing/path")
	// Move cwd to a place where ./image won't exist.
	dir := t.TempDir()
	prev, _ := os.Getwd()
	defer os.Chdir(prev)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	_, err := findImageDir("image-no-such-thing")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "searched:") {
		t.Errorf("error should list candidates: %v", err)
	}
}
