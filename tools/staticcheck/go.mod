// Pinned build of staticcheck for `make lint` and CI.
//
// This is its own module so the analyzer can be built against an x/tools
// newer than the one staticcheck v0.8.1 itself pins: Go 1.27.2 moved the
// compiler export-data format (version 5) and the pinned x/tools from April
// cannot read it, which made `go run honnef.co/go/tools/cmd/staticcheck@v0.8.1`
// fail on every package. Bump staticcheck here (and only here); the Makefile
// and .github/workflows/test.yml build from this module.
module drydock/tools/staticcheck

go 1.27.2

require honnef.co/go/tools v0.8.1

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
)
