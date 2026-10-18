// Package strictjson decodes exactly one JSON value from a body. The standard
// streaming decoder stops after the first value and ignores whatever follows,
// which lets two readers of the same bytes disagree about what the body says:
// `{"auto_approve":true} x` fails a whole-body json.Unmarshal (so a guard that
// probes it sees nothing) yet decodes fine through json.NewDecoder (so the
// server that acts on it sees auto_approve). Every drydock HTTP body is a
// single JSON object; anything after it is a malformed request.
package strictjson

import (
	"encoding/json"
	"errors"
	"io"
)

// ErrTrailingData reports bytes after the first JSON value.
var ErrTrailingData = errors.New("strictjson: trailing data after the JSON value")

// Decode reads one JSON value from r into v and fails if anything other than
// whitespace follows it.
func Decode(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return ErrTrailingData
		}
		// A syntax error after the value (e.g. `} x`) is trailing data too.
		return ErrTrailingData
	}
	return nil
}
