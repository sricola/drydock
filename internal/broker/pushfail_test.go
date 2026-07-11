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
		{"non-ff with 403 in id", "! [rejected]        agent/aaa403bbb -> agent/aaa403bbb (non-fast-forward)\nUpdates were rejected", reasonNonFastForward},
		{"non-ff with 503 in id", "error: failed to push some refs to 'https://github.com/o/r'\n ! [rejected] agent/dead503beef -> agent/dead503beef (fetch first)", reasonNonFastForward},
		{"real http 403", "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 403", reasonAuth},
		{"real http 503", "error: RPC failed; HTTP 503 curl 22 The requested URL returned error: 503\nfatal: the remote end hung up unexpectedly", reasonTransient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPushError(c.text); got != c.want {
				t.Errorf("classifyPushError = %q, want %q", got, c.want)
			}
		})
	}
}
