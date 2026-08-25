package object

import "testing"

func TestValidateLabels(t *testing.T) {
	ok := []map[string]string{
		nil,
		{"team": "billing"},
		{"app.kubernetes.io/part-of": "api", "tier": "backend_2"},
	}
	for _, labels := range ok {
		if err := ValidateLabels(labels); err != nil {
			t.Errorf("%v was refused: %v", labels, err)
		}
	}

	bad := []struct {
		labels map[string]string
		why    string
	}{
		{map[string]string{"Team": "billing"}, "uppercase key"},
		{map[string]string{"team": "Billing"}, "uppercase value"},
		{map[string]string{"-team": "billing"}, "leading dash"},
		{map[string]string{"team": ""}, "empty value"},
		{map[string]string{"drover.io/source": "x"}, "the reserved prefix"},
	}
	for _, c := range bad {
		if err := ValidateLabels(c.labels); err == nil {
			t.Errorf("%s was accepted: %v", c.why, c.labels)
		}
	}
}

func TestSelectorMatches(t *testing.T) {
	labels := map[string]string{"team": "billing", "tier": "backend"}
	cases := []struct {
		expr string
		want bool
	}{
		{"", true},
		{"team=billing", true},
		{"team==billing", true},
		{"team=payments", false},
		{"team!=payments", true},
		{"team!=billing", false},
		{"tier", true},
		{"owner", false},
		{"!owner", true},
		{"!team", false},
		{"team=billing,tier=backend", true},
		{"team=billing,tier=frontend", false},
	}
	for _, c := range cases {
		sel, err := ParseSelector(c.expr)
		if err != nil {
			t.Errorf("%q did not parse: %v", c.expr, err)
			continue
		}
		if got := sel.Matches(labels); got != c.want {
			t.Errorf("%q matched %v, want %v", c.expr, got, c.want)
		}
	}
}

// An absent label satisfies "!=", the same way it does in kubectl: "not on
// the billing team" includes "on no team at all".
func TestNotEqualsIncludesAbsent(t *testing.T) {
	sel, err := ParseSelector("team!=billing")
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(map[string]string{}) {
		t.Error("an object with no labels did not satisfy team!=billing")
	}
}

// A selector may name a label drover generated -- selecting on
// drover.io/source is the point of writing it.
func TestSelectorMayNameAGeneratedLabel(t *testing.T) {
	sel, err := ParseSelector("drover.io/source=repository")
	if err != nil {
		t.Fatalf("a generated label was refused in a selector: %v", err)
	}
	if !sel.Matches(map[string]string{"drover.io/source": "repository"}) {
		t.Error("it parsed but did not match")
	}
}

func TestBadSelectors(t *testing.T) {
	for _, expr := range []string{"team=billing,", "=billing", "team=BILLING", "!"} {
		if _, err := ParseSelector(expr); err == nil {
			t.Errorf("%q was accepted", expr)
		}
	}
}
