package main

import (
	"reflect"
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
