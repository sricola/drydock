package gateway

import "testing"

func TestStaticKey_Current(t *testing.T) {
	var c Credential = StaticKey("sk-ant-abc")
	got, err := c.Current()
	if err != nil || got != "sk-ant-abc" {
		t.Fatalf("Current() = %q, %v; want sk-ant-abc, nil", got, err)
	}
}
