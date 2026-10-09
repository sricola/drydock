//go:build tools

// Package tools pins the staticcheck build dependency; see go.mod.
package tools

import _ "honnef.co/go/tools/cmd/staticcheck"
