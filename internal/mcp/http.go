package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/notshekhar/drover/internal/httpguard"
)

// MaxRequestBytes caps one incoming HTTP message.
const MaxRequestBytes = 4 << 20

// HTTPHandler serves MCP over the Streamable HTTP transport, so an agent can
// point at a running `drover serve` instead of spawning a process.
//
// POST carries one JSON-RPC message and gets one JSON response back. GET opens
// the server-initiated event stream, which is how an HTTP client is told its
// tool list changed -- the same notification the stdio bridge sends down its
// own pipe.
func (s *Server) HTTPHandler() http.Handler {
	if s.hub == nil {
		s.hub = newHub()
	}
	r := s.router()

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A browser on any page can POST to a localhost port. Without an Origin
		// check, a visited web page could drive this endpoint and read every
		// repository drover holds -- the DNS-rebinding attack the transport
		// spec calls out for local servers.
		//
		// The engine's own guard applies the same rule to every route, so on a
		// running `drover serve` this is the second of two. It stays because it
		// belongs to the transport: the spec asks an MCP server to validate
		// Origin, and this handler should not depend on who mounted it.
		if !httpguard.OriginAllowed(req) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}

		switch req.Method {
		case http.MethodPost:
			s.handleHTTPPost(w, req, r)
		case http.MethodGet:
			// A GET that did not ask for the stream is a client that does not
			// know about it, or a person with a browser. Both are better off
			// with the old refusal than with a connection that hangs open.
			if !wantsEventStream(req) {
				w.Header().Set("Allow", "GET, POST")
				http.Error(w, "this endpoint accepts POST, or GET with Accept: text/event-stream for notifications", http.StatusMethodNotAllowed)
				return
			}
			s.handleStream(w, req)
		case http.MethodDelete:
			// Sessions are not used, so there is nothing to tear down. Answer
			// cleanly rather than 405, since a client ending a session is
			// being well-behaved.
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func (s *Server) handleHTTPPost(w http.ResponseWriter, req *http.Request, r *router) {
	body, err := io.ReadAll(io.LimitReader(req.Body, MaxRequestBytes))
	if err != nil {
		writeRPC(w, http.StatusOK, &response{JSONRPC: "2.0", Error: errf(CodeParseError, "could not read the request: %v", err)})
		return
	}

	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		writeRPC(w, http.StatusOK, &response{JSONRPC: "2.0", Error: errf(CodeInvalidRequest, "empty request")})
		return
	}

	// A batch is a JSON array. The 2025-06-18 revision dropped batching, but
	// older clients still send it, and answering one is cheaper than making
	// them care which revision they reached.
	if trimmed[0] == '[' {
		s.handleHTTPBatch(req.Context(), w, []byte(trimmed), r)
		return
	}

	var rpcReq request
	if err := json.Unmarshal([]byte(trimmed), &rpcReq); err != nil {
		writeRPC(w, http.StatusOK, &response{JSONRPC: "2.0", Error: errf(CodeParseError, "invalid JSON: %v", err)})
		return
	}

	resp := r.dispatch(req.Context(), &rpcReq)
	if resp == nil {
		// A notification gets no body. 202 is what the transport specifies.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

func (s *Server) handleHTTPBatch(ctx context.Context, w http.ResponseWriter, body []byte, r *router) {
	var reqs []request
	if err := json.Unmarshal(body, &reqs); err != nil {
		writeRPC(w, http.StatusOK, &response{JSONRPC: "2.0", Error: errf(CodeParseError, "invalid JSON: %v", err)})
		return
	}

	responses := make([]*response, 0, len(reqs))
	for i := range reqs {
		if resp := r.dispatch(ctx, &reqs[i]); resp != nil {
			responses = append(responses, resp)
		}
	}
	if len(responses) == 0 {
		// Every message was a notification.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(responses)
}

func writeRPC(w http.ResponseWriter, code int, resp *response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}
