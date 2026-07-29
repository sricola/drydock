package repokey

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"https://github.com/Owner/Repo.git": "github.com/Owner/Repo",
		"https://user:pass@github.com/o/r":  "github.com/o/r",
		"git@github.com:o/r.git":            "github.com/o/r",
		"ssh://git@GitHub.com/o/r":          "github.com/o/r",
		"https://gitlab.example.com/g/p/":   "gitlab.example.com/g/p",
		"github.com/o/r":                    "github.com/o/r",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalize_HostileInputsDoNotPanic(t *testing.T) {
	for _, in := range []string{"", ":", "http://", "git@", "a b c", "https://[::1", "%zz"} {
		_ = Normalize(in)
	}
}
