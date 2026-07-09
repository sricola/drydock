package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imageBuildHint must recognise two known Apple `container` failure modes and
// name the right cause: an empty build context (with the symlinked-path
// trigger called out when the context dir resolves elsewhere), and in-VM DNS
// failure when the host's resolvers are loopback proxies (WARP, dnscrypt,
// some VPNs). Anything else gets no hint so genuine Dockerfile/registry
// problems fall back to the raw error.
func TestImageBuildHint(t *testing.T) {
	emptyContext := "#3 transferring context: 2B done\n" +
		"#6 [linux/arm64/v8 1/3] COPY hello.sh /hello.sh\n" +
		`Error: failed to compute cache key: failed to calculate checksum of ref ...: "/hello.sh": not found`
	dnsFailure := `#6 7.058 Err:1 http://deb.debian.org/debian bookworm InRelease
#6 7.058   Temporary failure resolving 'deb.debian.org'
#6 ERROR: process "/bin/sh -c apt-get update" did not complete successfully: exit code: 100`

	realDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(realDir) // macOS tempdirs sit behind /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(resolved, "link")
	if err := os.Symlink(resolved, linkDir); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		out     string
		ctxDir  string
		want    []string // substrings that must appear
		wantNot []string // substrings that must not appear
	}{
		{
			name:   "empty context via symlinked dir names the symlink",
			out:    emptyContext,
			ctxDir: linkDir,
			want:   []string{"symlink", resolved},
		},
		{
			name:    "empty context via real dir keeps generic guidance",
			out:     emptyContext,
			ctxDir:  resolved,
			want:    []string{"container system stop"},
			wantNot: []string{"symlink"},
		},
		{
			name:   "dns failure names loopback resolvers and --dns remedy",
			out:    dnsFailure,
			ctxDir: resolved,
			want:   []string{"--dns", "loopback"},
		},
		{
			name:   "unrelated failure yields no hint",
			out:    "Error: pull access denied for private/image, repository does not exist",
			ctxDir: resolved,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageBuildHint(tc.out, tc.ctxDir)
			if len(tc.want) == 0 && got != "" {
				t.Fatalf("want empty hint, got %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("hint missing %q:\n%s", w, got)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("hint should not contain %q:\n%s", w, got)
				}
			}
		})
	}
}
