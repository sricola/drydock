// Package defaults holds build-time defaults embedded from the canonical
// config files in the repo root. Using //go:embed here (rather than a hand-
// maintained string constant) means the embedded bytes are always byte-equal
// to the committed source file — a test enforces this.
//
// Why a separate package? go:embed can only reach files within the embedding
// package's own directory tree. config/egress.yaml lives at the repo root,
// outside cmd/drydock, so a copy is kept here and kept in sync by a test
// (TestEgressYAMLMatchesSource). When config/egress.yaml changes, the test
// fails until this copy is updated too.
package defaults

import _ "embed"

// EgressYAML is the default egress allowlist written by `drydock init` when
// ~/.drydock/egress.yaml does not yet exist and no share-dir template is
// reachable. It is byte-equal to config/egress.yaml in the repo root.
//
//go:embed egress.yaml
var EgressYAML []byte
