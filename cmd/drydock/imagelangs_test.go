package main

import (
	"strings"
	"testing"
)

// Drift guard: every language the hardcoded imageLanguages table claims the
// sandbox ships must actually appear in image/Dockerfile, and where the
// Dockerfile states a version (node's FROM tag, ARG GO_VERSION) it must match
// the table. When this fails, image/Dockerfile changed — update imagelangs.go.
func TestImageLanguages_MatchesDockerfile(t *testing.T) {
	got, err := dockerfileLanguages("../../image/Dockerfile")
	if err != nil {
		t.Fatalf("dockerfileLanguages: %v", err)
	}
	for _, l := range imageLanguages() {
		v, ok := got[l.Name]
		if !ok {
			t.Errorf("table ships %s %s but image/Dockerfile does not mention it", l.Name, l.Version)
			continue
		}
		if v != "" && !strings.HasPrefix(l.Version, v) && !strings.HasPrefix(v, l.Version) {
			t.Errorf("table says %s %s but image/Dockerfile says %s", l.Name, l.Version, v)
		}
	}
}

func TestImageShips(t *testing.T) {
	for _, lang := range []string{"node", "python", "go"} {
		if _, ok := imageShips(lang); !ok {
			t.Errorf("imageShips(%s) = false, want true", lang)
		}
	}
	if _, ok := imageShips("rust"); ok {
		t.Error("imageShips(rust) = true, want false")
	}
}
