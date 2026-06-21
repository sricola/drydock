package main

import (
	"strings"
	"testing"
)

// imageBuildHint must recognise the Apple `container` empty-build-context
// signature (COPY of a present-on-disk file fails because the context never
// reached the builder) and point the operator at the runtime, not drydock.
func TestImageBuildHint_EmptyContextSignature(t *testing.T) {
	out := "#3 transferring context: 2B done\n" +
		"#6 [linux/arm64/v8 1/3] COPY hello.sh /hello.sh\n" +
		`Error: failed to compute cache key: failed to calculate checksum of ref ...: "/hello.sh": not found`
	hint := imageBuildHint(out)
	if hint == "" {
		t.Fatal("expected a hint for the empty-context build failure")
	}
	if !strings.Contains(strings.ToLower(hint), "container") {
		t.Errorf("hint should point at the container runtime, got: %q", hint)
	}
}

// An unrelated build failure gets no special hint — the caller falls back to
// the raw error so we don't mislabel genuine Dockerfile/registry problems.
func TestImageBuildHint_UnrelatedFailureHasNoHint(t *testing.T) {
	if h := imageBuildHint("Error: pull access denied for private/image, repository does not exist"); h != "" {
		t.Errorf("unrelated failure should not get the container-context hint, got: %q", h)
	}
}
