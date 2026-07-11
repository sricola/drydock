package broker

import "testing"

func TestClassifyPushError(t *testing.T) {
	cases := []struct {
		name string
		text string
		want pushReason
	}{
		{"non-ff rejected", "! [rejected]        agent/x -> agent/x (non-fast-forward)\nUpdates were rejected", reasonNonFastForward},
		{"fetch first", "error: failed to push some refs\nhint: Updates were rejected because the remote contains work; fetch first", reasonNonFastForward},
		{"dns", "fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com", reasonTransient},
		{"timeout", "fatal: unable to access '...': Failed to connect to github.com port 443: Connection timed out", reasonTransient},
		{"rpc", "error: RPC failed; curl 92 HTTP/2 stream 0 was not closed cleanly\nfatal: the remote end hung up unexpectedly", reasonTransient},
		{"auth failed", "remote: Support for password authentication was removed.\nfatal: Authentication failed for 'https://github.com/o/r/'", reasonAuth},
		{"permission", "ERROR: Permission to o/r.git denied to user.\nfatal: Could not read from remote repository.", reasonAuth},
		{"protected", "remote: error: GH006: Protected branch update failed for refs/heads/main.", reasonProtected},
		{"pre-receive", "remote: error: pre-receive hook declined", reasonProtected},
		{"unknown", "fatal: something nobody has ever seen before", reasonUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPushError(c.text); got != c.want {
				t.Errorf("classifyPushError = %q, want %q", got, c.want)
			}
		})
	}
}
