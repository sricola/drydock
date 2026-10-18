package strictjson

import (
	"errors"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	type body struct {
		A bool `json:"a"`
	}
	cases := []struct {
		in      string
		wantA   bool
		wantErr bool
		trail   bool
	}{
		{`{"a":true}`, true, false, false},
		{"  {\"a\":true}\n\t ", true, false, false},
		{`{"a":true} x`, false, true, true},
		{`{"a":true}{}`, false, true, true},
		{`{"a":true}` + "\n" + `[1]`, false, true, true},
		{`{"a":true}}`, false, true, true},
		{`{"a":`, false, true, false},
		{``, false, true, false},
		{`null`, false, false, false},
	}
	for _, c := range cases {
		var v body
		err := Decode(strings.NewReader(c.in), &v)
		if (err != nil) != c.wantErr {
			t.Errorf("Decode(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if errors.Is(err, ErrTrailingData) != c.trail {
			t.Errorf("Decode(%q) trailing = %v, want %v (err %v)", c.in, errors.Is(err, ErrTrailingData), c.trail, err)
		}
		if err == nil && v.A != c.wantA {
			t.Errorf("Decode(%q) a = %v, want %v", c.in, v.A, c.wantA)
		}
	}
}
