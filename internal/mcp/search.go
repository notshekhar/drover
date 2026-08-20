package mcp

import (
	"sort"
	"strings"
)

// Fuzzy matching for api_list's search argument.
//
// The point is that an agent looking for "the issues endpoint" should find
// `list-issues` whether it searches for "issues", "issue", "lsiss" or "list
// open issues on a repo" -- and should be able to search by anything the
// document says, not only by name. So every property a request carries is
// folded into one haystack, and a query matches when all of its terms hit
// somewhere in it.

// searchable is one candidate: what to rank, and the text to rank it against.
type searchable struct {
	// Name is weighted highest, since it is what a caller ultimately passes.
	Name string
	// Fields are every other property, already lowercased by the builder.
	Fields []string
}

// score returns how well this candidate matches the query, and whether it
// matched at all. Higher is better.
//
// Every term must match somewhere (AND, not OR): searching "issues prod"
// should find the issues request in the prod environment, not everything
// mentioning either word.
func (s searchable) score(query string) (int, bool) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return 0, true
	}

	name := strings.ToLower(s.Name)
	haystack := name
	if len(s.Fields) > 0 {
		haystack += " " + strings.Join(s.Fields, " ")
	}

	total := 0
	for _, term := range terms {
		best := 0
		switch {
		case name == term:
			best = 1000
		case strings.HasPrefix(name, term):
			best = 500
		case strings.Contains(name, term):
			best = 300
		case strings.Contains(haystack, term):
			best = 100
		case isSubsequence(term, name):
			// "lsiss" finding list-issues. Weakest signal, so it ranks below
			// anything that actually contains the term.
			best = 50
		default:
			return 0, false
		}
		total += best
	}

	// A shorter name matching the same terms is the more specific hit.
	total -= len(name)
	return total, true
}

// isSubsequence reports whether every character of needle appears in haystack
// in order. This is what makes an abbreviation work as a search.
func isSubsequence(needle, haystack string) bool {
	if needle == "" {
		return true
	}
	i := 0
	for j := 0; j < len(haystack) && i < len(needle); j++ {
		if haystack[j] == needle[i] {
			i++
		}
	}
	return i == len(needle)
}

// rank filters and orders candidates by a query. An empty query keeps
// everything in its original order, which is how api_list returns the full
// catalogue when no search is given.
func rank[T any](items []T, query string, key func(T) searchable) []T {
	if strings.TrimSpace(query) == "" {
		return items
	}

	type scored struct {
		item  T
		score int
		order int
	}
	var hits []scored
	for i, item := range items {
		if s, ok := key(item).score(query); ok {
			hits = append(hits, scored{item: item, score: s, order: i})
		}
	}

	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		// Ties keep the original order, so a listing is stable between calls.
		return hits[a].order < hits[b].order
	})

	out := make([]T, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.item)
	}
	return out
}
