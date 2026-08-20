package mcp

import "testing"

// Tool names use underscores; object names use dashes. The mapping has to
// round-trip or a call cannot find its object.
func TestToolNameRoundTrip(t *testing.T) {
	for _, name := range []string{"get-user", "api", "a-b-c", "x1"} {
		if got := objectName(toolSuffix(name)); got != name {
			t.Errorf("round trip of %q gave %q", name, got)
		}
	}
}
func TestStringifyCoercesArguments(t *testing.T) {
	// JSON has one number type, and an id of 42 must not become "42.0" in a URL.
	if got := stringify(float64(42)); got != "42" {
		t.Errorf("stringify(42) = %q", got)
	}
	if got := stringify(1.5); got != "1.5" {
		t.Errorf("stringify(1.5) = %q", got)
	}
	if got := stringify("x"); got != "x" {
		t.Errorf("stringify(\"x\") = %q", got)
	}
	if got := stringify(true); got != "true" {
		t.Errorf("stringify(true) = %q", got)
	}
	if got := stringify(nil); got != "" {
		t.Errorf("stringify(nil) = %q", got)
	}
}
