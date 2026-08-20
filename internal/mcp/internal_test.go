package mcp

import "testing"

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

// The search has to find a request by anything the document says, not only by
// its name, or a large collection is unusable.
func TestSearchRanking(t *testing.T) {
	items := []searchable{
		{Name: "list-issues", Fields: []string{"get", "{{baseurl}}/repos/{owner}/{repo}/issues", "github", "owner repo state"}},
		{Name: "get-repo", Fields: []string{"get", "{{baseurl}}/repos/{owner}/{repo}", "github", "fetch one github repository by owner and name"}},
		{Name: "get-user", Fields: []string{"get", "{{baseurl}}/users/{id}", "prod", "fetch one user by id"}},
	}
	key := func(s searchable) searchable { return s }

	cases := map[string]string{
		"issues":        "list-issues", // in the name
		"list-issues":   "list-issues", // exact
		"repository":    "get-repo",    // only in the description
		"user":          "get-user",
		"lsiss":         "list-issues", // an abbreviation, via subsequence
		"github issues": "list-issues", // two terms, both must hit
	}
	for query, want := range cases {
		got := rank(items, query, key)
		if len(got) == 0 {
			t.Errorf("search %q found nothing", query)
			continue
		}
		if got[0].Name != want {
			t.Errorf("search %q ranked %q first, want %q", query, got[0].Name, want)
		}
	}

	// Every term must match: this is an AND, not an OR.
	if got := rank(items, "issues nonexistentword", key); len(got) != 0 {
		t.Errorf("search with an unmatched term returned %d results, want 0", len(got))
	}

	// No search returns everything, in the original order.
	all := rank(items, "", key)
	if len(all) != len(items) || all[0].Name != "list-issues" {
		t.Errorf("an empty search changed the listing: %+v", all)
	}
	if all := rank(items, "   ", key); len(all) != len(items) {
		t.Errorf("a whitespace search filtered the listing")
	}
}

func TestIsSubsequence(t *testing.T) {
	if !isSubsequence("lsiss", "list-issues") {
		t.Error("lsiss should be a subsequence of list-issues")
	}
	if !isSubsequence("", "anything") {
		t.Error("the empty string is a subsequence of everything")
	}
	if isSubsequence("zzz", "list-issues") {
		t.Error("zzz is not a subsequence of list-issues")
	}
	// Order matters, or the match means nothing.
	if isSubsequence("seussi", "list-issues") {
		t.Error("characters out of order should not match")
	}
}
