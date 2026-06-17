package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseEgressExtras(t *testing.T) {
	cases := []struct {
		in      []string
		want    []reqDomain
		wantErr bool
	}{
		{nil, nil, false},
		{[]string{}, nil, false},
		{
			in: []string{"api.example.com:443"},
			want: []reqDomain{
				{Host: "api.example.com", Ports: []int{443}},
			},
		},
		{
			in: []string{"a.example.com:443,8443", "b.example.com:80"},
			want: []reqDomain{
				{Host: "a.example.com", Ports: []int{443, 8443}},
				{Host: "b.example.com", Ports: []int{80}},
			},
		},
		// Trims whitespace inside the port list.
		{
			in: []string{"a.example.com:443, 8443"},
			want: []reqDomain{
				{Host: "a.example.com", Ports: []int{443, 8443}},
			},
		},

		// Errors
		{in: []string{"no-port"}, wantErr: true},
		{in: []string{":443"}, wantErr: true},
		{in: []string{"host:"}, wantErr: true},
		{in: []string{"host:abc"}, wantErr: true},
		{in: []string{"host:443,xyz"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseEgressExtras(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEgressExtras(%v) want err, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEgressExtras(%v) unexpected err: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseEgressExtras(%v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestRepeatedFlag(t *testing.T) {
	var r repeatedFlag
	if err := r.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("b"); err != nil {
		t.Fatal(err)
	}
	if got := r.String(); got != "a,b" {
		t.Errorf("String = %q", got)
	}
	if len(r) != 2 || r[0] != "a" || r[1] != "b" {
		t.Errorf("slice = %v", r)
	}
}

// Model must round-trip through the request JSON the same way the other
// optional fields do (omitempty), so a Model-less submit doesn't pollute
// audit logs with empty-string fields and an explicit Model lands intact.
func TestTaskRequest_ModelOmitemptyAndRoundtrip(t *testing.T) {
	empty, err := json.Marshal(taskRequest{RepoRef: "git@github.com:o/r", Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), `"model"`) {
		t.Errorf("empty Model should be omitted, got %s", empty)
	}
	set, err := json.Marshal(taskRequest{RepoRef: "git@github.com:o/r", Instruction: "x", Model: "claude-opus-4-8"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(set), `"model":"claude-opus-4-8"`) {
		t.Errorf("Model not emitted: %s", set)
	}
}
