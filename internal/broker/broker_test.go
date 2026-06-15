package broker

import (
	"slices"
	"testing"
)

func TestDropEmptyNetwork(t *testing.T) {
	in := []string{"run", "--network", "", "--user", "agent"}
	got := dropEmptyNetwork(in)
	want := []string{"run", "--user", "agent"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestDropEmptyNetwork_KeepsNamed(t *testing.T) {
	in := []string{"run", "--network", "egress-x", "--rm"}
	got := dropEmptyNetwork(in)
	if !slices.Contains(got, "egress-x") {
		t.Errorf("named network dropped: %v", got)
	}
}
