package claudekeychain

import (
	"errors"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	raw := []byte(`{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":1750000000000}}`)
	snap, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Access != "a1" || snap.Refresh != "r1" {
		t.Fatalf("snap=%+v", snap)
	}
	if want := time.UnixMilli(1750000000000); !snap.Expiry.Equal(want) {
		t.Fatalf("Expiry = %v, want %v", snap.Expiry, want)
	}
}

func TestParse_Rejects(t *testing.T) {
	for name, raw := range map[string]string{
		"not logged in":      `{}`,
		"empty access token": `{"claudeAiOauth":{"accessToken":"","refreshToken":"r1","expiresAt":1750000000000}}`,
		"malformed json":     `{not valid json`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestSnapshot_CollapsesFailuresToNotOK(t *testing.T) {
	orig := readBlob
	defer func() { readBlob = orig }()
	readBlob = func() ([]byte, error) { return nil, errors.New("security: item not found") }
	if _, ok := Snapshot(); ok {
		t.Fatal("a Keychain read failure must report ok=false")
	}
	readBlob = func() ([]byte, error) { return []byte(`{}`), nil }
	if _, ok := Snapshot(); ok {
		t.Fatal("a not-logged-in blob must report ok=false")
	}
	readBlob = func() ([]byte, error) {
		return []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1750000000000}}`), nil
	}
	snap, ok := Snapshot()
	if !ok || snap.Access != "a" || snap.Refresh != "r" {
		t.Fatalf("Snapshot=%+v ok=%v", snap, ok)
	}
}

func TestRead_ErrorCarriesNoTokenMaterial(t *testing.T) {
	orig := readBlob
	defer func() { readBlob = orig }()
	readBlob = func() ([]byte, error) { return nil, errors.New("boom") }
	_, err := Read()
	if err == nil || err.Error() != "could not read Claude credentials from Keychain, run `claude login` first" {
		t.Fatalf("err=%v", err)
	}
}
