package egress

import (
	"strings"
	"testing"
)

// Exact limits: 253 characters is the longest valid hostname, ports run 1..65535.
func TestValidateHost_LengthBoundary(t *testing.T) {
	label := strings.Repeat("a", 63)
	host253 := label + "." + label + "." + label + "." + strings.Repeat("a", 61) // 63*3 + 3 dots + 61 = 253
	if len(host253) != 253 {
		t.Fatalf("fixture length %d", len(host253))
	}
	if err := ValidateHost(host253); err != nil {
		t.Errorf("253-char host rejected: %v", err)
	}
	if err := ValidateHost(host253 + "a"); err == nil {
		t.Error("254-char host accepted")
	}
}

func TestValidatePorts_RangeBoundary(t *testing.T) {
	for _, ok := range []int{1, 65535} {
		if err := ValidatePorts([]int{ok}); err != nil {
			t.Errorf("port %d rejected: %v", ok, err)
		}
	}
	for _, bad := range []int{0, 65536, -1} {
		if err := ValidatePorts([]int{bad}); err == nil {
			t.Errorf("port %d accepted", bad)
		}
	}
}
