package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestTotalCoverage_WeightsByStatements(t *testing.T) {
	profile := "mode: atomic\n" +
		"a.go:1.1,2.2 3 1\n" + // 3 statements covered
		"a.go:3.1,4.2 1 0\n" + // 1 uncovered
		"b.go:1.1,9.9 6 7\n" // 6 covered
	got, err := totalCoverage(bufio.NewScanner(strings.NewReader(profile)))
	if err != nil {
		t.Fatal(err)
	}
	if want := 90.0; got != want {
		t.Fatalf("total = %.2f, want %.2f (9 of 10 statements)", got, want)
	}
}

func TestTotalCoverage_RejectsMalformedAndEmpty(t *testing.T) {
	if _, err := totalCoverage(bufio.NewScanner(strings.NewReader("mode: set\nnot a profile line\n"))); err == nil {
		t.Error("malformed line accepted")
	}
	if _, err := totalCoverage(bufio.NewScanner(strings.NewReader("mode: set\n"))); err == nil {
		t.Error("empty profile accepted")
	}
}
