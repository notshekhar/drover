package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/notshekhar/drover/internal/object"
)

// drover's tool list is not fixed once a session starts. Applying the first
// SQLConnection makes sql_query appear, the first HTTPRequest adds three
// tools, a health check going red takes one away -- and drover applies files
// dropped in its data directory on its own, so this happens without anyone
// restarting anything.
//
// A client that cached tools/list at connect keeps a list that is wrong until
// it reconnects. tools/listChanged exists for exactly this, and drover has
// advertised it since the tool set was built; this is the half that was
// missing.

// toolChangePoll is how often the stdio bridge re-reads the tool list.
//
// Polling rather than a subscription, for the same reason the config watcher
// polls: the engine may be another process or another machine, and a poll over
// loopback costs three small reads. A few seconds of staleness is invisible
// next to how long a model takes to decide it needs a tool.
var toolChangePoll = 5 * time.Second

// watchToolChanges emits notifications/tools/list_changed whenever the
// advertised tools stop matching what the peer was last told.
//
// It must not start before the client's initialized notification: a
// server-initiated message during the handshake is a protocol violation, and
// some clients treat one as a fatal desync.
//
// The baseline is a parameter rather than the loop's first act, because a
// baseline taken inside the goroutine is taken at an unknown time. Anything
// applied between the caller's decision to watch and the scheduler getting
// here would be folded into the baseline and could then never look like a
// change. See startToolWatch, which takes it synchronously.
func (s *Server) watchToolChanges(notify func(method string, params any), baseline string, done <-chan struct{}) {
	last := baseline

	ticker := time.NewTicker(toolChangePoll)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			current, ok := s.toolFingerprint()
			if !ok {
				// A momentary failure to reach the engine collapses the list
				// to the file tools. Announcing that would tell the client to
				// re-list, and it would cache the degraded answer. Say nothing
				// and try again.
				continue
			}
			if current == last {
				continue
			}
			// A baseline taken while the engine was down is not a change.
			if last != "" {
				notify("notifications/tools/list_changed", nil)
			}
			last = current
		}
	}
}

// toolFingerprint hashes the tool list as the client would receive it, and
// reports whether the engine could be read at all.
//
// Descriptions are part of the hash, not just names. They carry the catalogues
// -- the databases inside sql_query's description, the counts inside
// api_list's -- so a connection appearing changes the payload the client
// caches even though the tool set is the same size.
func (s *Server) toolFingerprint() (string, bool) {
	// The tool builders swallow their errors and return no tools, which is
	// right for serving a list but would make an unreachable engine look like
	// an empty one here. Ask a question whose failure is visible first.
	if _, err := s.Backend.List(object.KindRepository); err != nil {
		return "", false
	}

	result, rpcErr := s.listTools(nil)
	if rpcErr != nil {
		return "", false
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return string(sum[:]), true
}
