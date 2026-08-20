package main

import "testing"

func TestVersionGreater(t *testing.T) {
	newer := [][2]string{
		{"v0.4.2", "v0.4.1"},
		{"v0.5.0", "v0.4.9"},
		{"v1.0.0", "v0.99.99"},
		{"0.4.2", "0.4.1"},  // the v is optional on either side
		{"v0.4.2", "0.4.1"}, // and may be mixed
		{"v0.2.0", "v0.1.9"},
		{"v0.4.10", "v0.4.9"}, // numeric, not lexical: 10 > 9
	}
	for _, c := range newer {
		if !versionGreater(c[0], c[1]) {
			t.Errorf("versionGreater(%q, %q) = false, want true", c[0], c[1])
		}
	}

	notNewer := [][2]string{
		{"v0.4.1", "v0.4.2"},
		{"v0.4.1", "v0.4.1"}, // equal is not newer, or every check offers an upgrade
		{"v0.4.9", "v0.5.0"},
		{"v0.9.9", "v1.0.0"},
	}
	for _, c := range notNewer {
		if versionGreater(c[0], c[1]) {
			t.Errorf("versionGreater(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

// A build from source reports "dev", and should always be told an upgrade is
// available rather than being compared as though it were 0.0.0.
func TestDevVersionIsAlwaysBehind(t *testing.T) {
	if !versionGreater("v0.1.0", "dev") {
		t.Error("a release should be newer than a dev build")
	}
	if versionGreater("dev", "v0.1.0") {
		t.Error("a dev build should not be treated as newer than a release")
	}
}

// A pre-release suffix must not make the comparison fall over; it is dropped
// rather than compared, which is more machinery than a self-updater needs.
func TestVersionSuffixesAreIgnored(t *testing.T) {
	if versionGreater("v0.4.1-rc1", "v0.4.1") {
		t.Error("a suffix should not make a version newer")
	}
	if !versionGreater("v0.4.2-rc1", "v0.4.1") {
		t.Error("the numeric part should still win")
	}
}

func TestTagFromURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/notshekhar/drover/releases/tag/v0.4.2": "v0.4.2",
		"https://github.com/notshekhar/drover/releases/tag/v1.0.0": "v1.0.0",
		// Not a tag url: the redirect landed somewhere unexpected, and
		// guessing would give the installer a nonsense version to fetch.
		"https://github.com/notshekhar/drover/releases":        "",
		"https://github.com/notshekhar/drover/releases/latest": "",
		"": "",
	}
	for url, want := range cases {
		if got := tagFromURL(url); got != want {
			t.Errorf("tagFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestSplitVersion(t *testing.T) {
	cases := map[string][3]int{
		"v1.2.3":     {1, 2, 3},
		"1.2.3":      {1, 2, 3},
		"v0.4.10":    {0, 4, 10},
		"v1.2":       {1, 2, 0},
		"v1":         {1, 0, 0},
		"v1.2.3-rc1": {1, 2, 3},
	}
	for in, want := range cases {
		if got := splitVersion(in); got != want {
			t.Errorf("splitVersion(%q) = %v, want %v", in, got, want)
		}
	}
}
