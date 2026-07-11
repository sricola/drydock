package broker

import "strings"

// pushReason is the classified cause of a git-push failure. Its string value is
// what the audit and the stream event report.
type pushReason string

const (
	reasonNonFastForward pushReason = "non_fast_forward"
	reasonTransient      pushReason = "transient"
	reasonAuth           pushReason = "auth"
	reasonProtected      pushReason = "protected"
	reasonUnknown        pushReason = "unknown"
)

// classifyPushError maps git's combined stderr (carried in the push error) to a
// reason. Order matters: protected and auth are checked before the generic
// transient/non-ff matchers so a specific server rejection is not misread. An
// unrecognized failure is reasonUnknown, which the recovery loop treats as
// terminal: never retry a failure we do not understand.
func classifyPushError(errText string) pushReason {
	s := strings.ToLower(errText)
	switch {
	case contains(s, "gh006", "protected branch", "pre-receive hook declined"):
		return reasonProtected
	case contains(s, "authentication failed", "could not read username",
		"permission to", "permission denied", "access denied",
		"error: 403", "http 403", "403 forbidden", "returned error: 403",
		"invalid username or password"):
		return reasonAuth
	case contains(s, "non-fast-forward", "! [rejected]", "fetch first",
		"updates were rejected"):
		return reasonNonFastForward
	case contains(s, "could not resolve host", "connection timed out",
		"connection reset", "could not read from remote", "rpc failed",
		"early eof", "timed out", "failed to connect",
		"error: 500", "error: 502", "error: 503", "error: 504",
		"http 500", "http 502", "http 503",
		"returned error: 500", "returned error: 502", "returned error: 503",
		"tls", "the remote end hung up"):
		return reasonTransient
	default:
		return reasonUnknown
	}
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
