package main

import (
	"strings"
	"testing"
)

// The broker writes a {"type":"drydock_task",...} line; retry must recover the
// invocation from it. This asserts the round-trip (broker schema → taskRequest).
func TestReadInvocation_RoundTrip(t *testing.T) {
	// Mirrors internal/broker's persisted line.
	trace := `{"type":"drydock_meta","subscription":false,"sensitive":false}
{"type":"drydock_task","repo_ref":"https://github.com/o/r","instruction":"do the thing","agent":"codex","model":"gpt-x","platform":"github","egress_extra":[{"host":"api.example.com","ports":[443]}],"draft":true,"sensitive":false}
{"type":"result","subtype":"success"}
`
	req, ok := readInvocation(strings.NewReader(trace))
	if !ok {
		t.Fatal("expected to find the drydock_task invocation line")
	}
	if req.RepoRef != "https://github.com/o/r" || req.Instruction != "do the thing" {
		t.Errorf("repo/instruction not recovered: %+v", req)
	}
	if req.Agent != "codex" || req.Model != "gpt-x" || req.Platform != "github" || !req.Draft {
		t.Errorf("flags not recovered: %+v", req)
	}
	if len(req.EgressExtra) != 1 || req.EgressExtra[0].Host != "api.example.com" || req.EgressExtra[0].Ports[0] != 443 {
		t.Errorf("egress_extra not recovered: %+v", req.EgressExtra)
	}
}

func TestReadInvocation_MissingLine(t *testing.T) {
	if _, ok := readInvocation(strings.NewReader(`{"type":"result","subtype":"success"}` + "\n")); ok {
		t.Error("expected ok=false when no drydock_task line is present")
	}
}
