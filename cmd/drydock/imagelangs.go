package main

import (
	"os"
	"strings"
)

// imageLang is one language toolchain the sandbox image ships.
type imageLang struct {
	Name    string
	Version string
}

// imageLanguages is the hardcoded table of toolchains baked into
// image/Dockerfile. TestImageLanguages_MatchesDockerfile guards it against
// drifting from the Dockerfile — when the image bumps a version or gains a
// language, update this table.
func imageLanguages() []imageLang {
	return []imageLang{
		{Name: "node", Version: "22"},
		{Name: "python", Version: "3.11"},
		{Name: "go", Version: "1.26.5"},
	}
}

// imageShips reports whether the sandbox image ships the named language
// toolchain, and its version if so.
func imageShips(lang string) (version string, ok bool) {
	for _, l := range imageLanguages() {
		if l.Name == lang {
			return l.Version, true
		}
	}
	return "", false
}

// dockerfileLanguages greps a Dockerfile for the shipped-language markers the
// drift test keys on: the `FROM node:<tag>` base, the apt `python3` install,
// and the `ARG GO_VERSION=` pin. Returns language -> version where the line
// states one ("" when only presence is detectable, as for Debian's python3).
func dockerfileLanguages(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	langs := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "FROM ") {
			if _, tag, ok := strings.Cut(t, "node:"); ok {
				langs["node"] = leadingVersion(tag)
			}
		}
		if v, ok := strings.CutPrefix(t, "ARG GO_VERSION="); ok {
			langs["go"] = leadingVersion(v)
		}
		if strings.Contains(t, "python3") {
			if _, seen := langs["python"]; !seen {
				langs["python"] = "" // apt package: presence only, no version literal
			}
		}
	}
	return langs, nil
}

// leadingVersion returns the leading dotted-numeric run of s ("22" from
// "22-bookworm-slim@sha256:...", "1.26.5" from "1.26.5").
func leadingVersion(s string) string {
	end := 0
	for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	return s[:end]
}
