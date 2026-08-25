package activity

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Hotspot is one path agents keep coming back to.
type Hotspot struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Reads      int    `json:"reads"`
}

// EmptySearch is a pattern that has been searched for repeatedly and found
// nothing.
type EmptySearch struct {
	Pattern string `json:"pattern"`
	Times   int    `json:"times"`
}

// hotspotScan caps how much of the log one summary reads. The ledger is a
// local sqlite file and this runs on a connection handshake, so it is bounded
// by design rather than by hope.
const hotspotScan = 5000

// Hotspots reports the files agents actually read, per repository.
//
// This is an index built from behaviour rather than from embeddings, and it
// costs one query against a file drover was already writing. What it is not
// is a recommendation: "agents most often read" is a fact about the past,
// and phrasing it as advice would give a model a shortcut it has not earned
// and stop it looking anywhere else.
//
// The result is capped and windowed for the same reason. An uncapped list
// feeds back on itself -- the files that were read stay read, because they
// are the ones the handshake keeps naming.
func (l *Ledger) Hotspots(ctx context.Context, window time.Duration, perRepo int) ([]Hotspot, error) {
	if l == nil || l.db == nil {
		return nil, nil
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT repository, args FROM calls
		 WHERE tool = 'read' AND outcome = 'ok' AND at >= ?
		 ORDER BY at DESC LIMIT ?`,
		time.Now().Add(-window).UTC().Format(time.RFC3339Nano), hotspotScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]map[string]int{}
	for rows.Next() {
		var repo string
		var raw []byte
		if err := rows.Scan(&repo, &raw); err != nil {
			return nil, err
		}
		path := argString(raw, "path")
		if path == "" {
			continue
		}
		if repo == "" {
			repo, _, _ = strings.Cut(path, "/")
		}
		if counts[repo] == nil {
			counts[repo] = map[string]int{}
		}
		counts[repo][path]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []Hotspot
	for repo, paths := range counts {
		var local []Hotspot
		for path, n := range paths {
			local = append(local, Hotspot{Repository: repo, Path: path, Reads: n})
		}
		sort.Slice(local, func(i, j int) bool {
			if local[i].Reads != local[j].Reads {
				return local[i].Reads > local[j].Reads
			}
			return local[i].Path < local[j].Path
		})
		if len(local) > perRepo {
			local = local[:perRepo]
		}
		out = append(out, local...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repository != out[j].Repository {
			return out[i].Repository < out[j].Repository
		}
		return out[i].Reads > out[j].Reads
	})
	return out, nil
}

// EmptySearches reports patterns that were searched for and found nothing,
// more than once.
//
// A recurring empty grep is usually a repository that should be in the
// warehouse and is not. That is a fact about what the warehouse is missing,
// which nothing else in drover can tell you.
func (l *Ledger) EmptySearches(ctx context.Context, window time.Duration, limit int) ([]EmptySearch, error) {
	if l == nil || l.db == nil {
		return nil, nil
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT args FROM calls
		 WHERE tool = 'grep' AND outcome = 'ok' AND summary LIKE '0 matches%' AND at >= ?
		 ORDER BY at DESC LIMIT ?`,
		time.Now().Add(-window).UTC().Format(time.RFC3339Nano), hotspotScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if p := argString(raw, "pattern"); p != "" {
			counts[p]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []EmptySearch
	for pattern, n := range counts {
		if n < 2 {
			continue // once is a typo; twice is a gap
		}
		out = append(out, EmptySearch{Pattern: pattern, Times: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Times != out[j].Times {
			return out[i].Times > out[j].Times
		}
		return out[i].Pattern < out[j].Pattern
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// argString pulls one recorded argument out of the stored JSON.
//
// Decoding in Go rather than with json_extract in SQL: the arguments are
// already redacted on the way in, the volume is bounded by hotspotScan, and
// this keeps the query working on any sqlite build.
func argString(raw []byte, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}
