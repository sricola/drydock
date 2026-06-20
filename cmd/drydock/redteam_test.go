package main

import "testing"

func TestA1Verdict(t *testing.T) {
	const sentinel = "sk-ant-SENTINEL-DO-NOT-LEAK"
	cases := []struct {
		name   string
		vmEnv  string
		wantOK bool
	}{
		{"real key leaked", "FOO=bar\nANTHROPIC_AUTH_TOKEN=tok_abc\n" + sentinel, false},
		{"only bearer present", "ANTHROPIC_BASE_URL=http://gw\nANTHROPIC_AUTH_TOKEN=tok_abc", true},
		{"no bearer at all", "FOO=bar\nPATH=/usr/bin", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, _ := a1Verdict(c.vmEnv, sentinel)
			if ok != c.wantOK {
				t.Errorf("a1Verdict ok=%v, want %v", ok, c.wantOK)
			}
		})
	}
}

func TestA2Verdict(t *testing.T) {
	cases := []struct {
		name   string
		probe  string
		wantOK bool
	}{
		{"all blocked", "HTTPS:blocked\nDNS:blocked\nDIRECTIP:blocked", true},
		{"https reachable", "HTTPS:reachable\nDNS:blocked\nDIRECTIP:blocked", false},
		{"dns resolves", "HTTPS:blocked\nDNS:resolved\nDIRECTIP:blocked", false},
		{"missing vector line", "HTTPS:blocked\nDNS:blocked", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, _ := a2Verdict(c.probe)
			if ok != c.wantOK {
				t.Errorf("a2Verdict ok=%v, want %v", ok, c.wantOK)
			}
		})
	}
}

func TestA7Verdict(t *testing.T) {
	const secret = "REDTEAM-A7-SECRET"
	cases := []struct {
		name   string
		probe  string
		wantOK bool
	}{
		{"state gone", "absent", true},
		{"secret carried over", secret, false},
		{"unexpected output", "permission denied", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, _ := a7Verdict(c.probe, secret)
			if ok != c.wantOK {
				t.Errorf("a7Verdict ok=%v, want %v", ok, c.wantOK)
			}
		})
	}
}
