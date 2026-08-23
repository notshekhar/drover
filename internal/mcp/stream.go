package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The Streamable HTTP transport does have a channel for a server-initiated
// message, and drover used to answer it with 405: the client opens a GET on
// the same endpoint and the server writes Server-Sent Events down it. The
// stdio bridge has announced tools/listChanged since the tool set was built,
// and an HTTP-connected agent -- the transport drover recommends first -- was
// left re-listing only when it happened to reconnect.
//
// One hub serves every listener. Notifications here are broadcasts about the
// engine, not answers to anybody's request, so there is nothing to route: a
// tool list that changed changed for all of them.

// streamPing keeps an idle stream from being reaped by anything in the middle.
// Loopback has nothing in the middle, but a comment frame every half minute
// costs two bytes and makes a stalled connection visible as one.
const streamPing = 30 * time.Second

// hub fans server-initiated messages out to the open GET streams.
type hub struct {
	mu   sync.Mutex
	subs map[int]chan []byte
	next int

	// watching is the tool watcher, started with the first listener and
	// stopped with the last. An engine nobody is streaming from does not poll:
	// the watcher exists to tell somebody, and there is nobody.
	watching bool
	stop     chan struct{}
}

func newHub() *hub { return &hub{subs: map[int]chan []byte{}} }

// subscribe returns a channel of framed messages and a function that closes it.
func (h *hub) subscribe() (<-chan []byte, func()) {
	// Buffered, and a slow reader is dropped rather than waited on -- a
	// blocked client must not be able to stall the watcher for everyone else.
	ch := make(chan []byte, 8)

	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
		h.mu.Unlock()
	}
}

func (h *hub) broadcast(method string, params any) {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- data:
		default: // a listener that cannot keep up misses this one
		}
	}
}

func (h *hub) listeners() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// handleStream answers a GET with an event stream.
//
// The context is the request's, so the stream ends when the client hangs up --
// which is also what retires the watcher, once the last one has gone.
func (s *Server) handleStream(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "this server cannot stream", http.StatusInternalServerError)
		return
	}

	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	s.startHubWatch()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(streamPing)
	defer ping.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case data, open := <-ch:
			if !open {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// startHubWatch runs the tool watcher while at least one stream is open.
//
// It deliberately does not take the opening request's context. A second
// listener arriving after the first has gone must get a watcher again, and by
// then the first one's context is long dead -- so the watcher's own stop
// channel governs it, and the tick loop asks whether anybody is still there.
func (s *Server) startHubWatch() {
	s.hub.mu.Lock()
	if s.hub.watching {
		s.hub.mu.Unlock()
		return
	}
	s.hub.watching = true
	s.hub.stop = make(chan struct{})
	stop := s.hub.stop
	s.hub.mu.Unlock()

	// Taken synchronously, for the reason startToolWatch spells out: a
	// baseline read inside the goroutine is read at an unknown time, and
	// anything applied in between is folded into it and never seen again.
	//
	// context.Background rather than the request's, for the same reason: this
	// outlives the request that started it, and a stopped watcher is already
	// the thing that ends the work.
	baseline, _ := s.toolFingerprint(context.Background())

	go func() {
		defer func() {
			s.hub.mu.Lock()
			s.hub.watching = false
			s.hub.mu.Unlock()
		}()
		s.watchToolChanges(context.Background(), s.hub.broadcast, baseline, stop, s.hubIsIdle)
	}()
}

// hubIsIdle ends the watch once the last stream has closed. Checked on the
// tick rather than in unsubscribe, so a client that reconnects between two
// ticks keeps the watcher it already had instead of stopping one and starting
// another -- and a restart would take a fresh baseline, silently swallowing
// anything applied across the gap.
func (s *Server) hubIsIdle() bool {
	if s.hub.listeners() > 0 {
		return false
	}
	// watching is cleared here and not only in the goroutine's defer: the
	// defer runs after this returns, and a listener arriving in that window
	// would see watching still true and get no watcher at all.
	s.hub.mu.Lock()
	s.hub.watching = false
	s.hub.stop = nil
	s.hub.mu.Unlock()
	return true
}

// wantsEventStream reports whether a GET is asking for the notification
// stream. A GET without it is a client that does not know about the stream, or
// a person with a browser, and both are better served by the old 405 than by a
// connection that hangs open forever.
func wantsEventStream(req *http.Request) bool {
	return strings.Contains(strings.ToLower(req.Header.Get("Accept")), "text/event-stream")
}
