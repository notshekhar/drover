package main

import (
	"reflect"
	"testing"
)

// Flags after positional arguments must still be seen. Go's flag package
// stops at the first positional, which silently dropped -o in
// `drover get repository api -o yaml`.
func TestSplitArgs(t *testing.T) {
	vf := clientFlags("o")

	cases := []struct {
		name           string
		in             []string
		wantFlags      []string
		wantPositional []string
	}{
		{
			name:           "flag after positionals",
			in:             []string{"repository", "api", "-o", "yaml"},
			wantFlags:      []string{"-o", "yaml"},
			wantPositional: []string{"repository", "api"},
		},
		{
			name:           "flag before positionals",
			in:             []string{"-o", "json", "repository"},
			wantFlags:      []string{"-o", "json"},
			wantPositional: []string{"repository"},
		},
		{
			name:           "equals form keeps its own value",
			in:             []string{"repository", "--o=yaml"},
			wantFlags:      []string{"--o=yaml"},
			wantPositional: []string{"repository"},
		},
		{
			name:           "value flag consumes the next token even if it looks like a word",
			in:             []string{"--server", "http://x", "repository", "api"},
			wantFlags:      []string{"--server", "http://x"},
			wantPositional: []string{"repository", "api"},
		},
		{
			name:           "double dash ends flags",
			in:             []string{"repository", "--", "-weird-name"},
			wantFlags:      nil,
			wantPositional: []string{"repository", "-weird-name"},
		},
		{
			name:           "bare dash is positional, for stdin",
			in:             []string{"-f", "-"},
			wantFlags:      []string{"-f", "-"},
			wantPositional: nil,
		},
		{
			name:           "no args",
			in:             nil,
			wantFlags:      nil,
			wantPositional: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags, positional := splitArgs(tc.in, map[string]bool{"o": true, "server": true, "f": true, "data-dir": true, "config": true})
			if !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
			if !reflect.DeepEqual(positional, tc.wantPositional) {
				t.Errorf("positional = %q, want %q", positional, tc.wantPositional)
			}
		})
	}

	// The shared set really does cover the client flags.
	for _, name := range []string{"data-dir", "config", "server", "o"} {
		if !vf[name] {
			t.Errorf("clientFlags is missing %q", name)
		}
	}
}
