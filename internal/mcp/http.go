package mcp

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// MaxRequestBytes caps one incoming HTTP message.
const MaxRequestBytes = 4 << 20

// HTTPHandler serves MCP over the Streamable HTTP transport, so an agent can
// point at a running `drover serve` instead of spawning a process.
//
// POST carries one JSON-RPC message and gets one JSON response back. GET would
// open a server-initiated event stream; drover never initiates anything, so it
// answers 405, which the transport explicitly allows.
func (s *Server) HTTPHandler() http.Handler {
	r := s.router()

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A browser on any page can POST to a localhost port. Without an Origin
		// check, a visited web page could drive this endpoint and read every
		// repository drover holds -- the DNS-rebinding attack the transport
		// spec calls out for local servers. drover has no auth by design, so
		// this check is the whole defence.
		if !originAllowed(req) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}

		switch req.Method {
		case http.MethodPost:
			s.handleHTTPPost(w, req, r)
		case http.MethodGet:
			// No server-initiated stream to offer.
			w.Header().Set("Allow", "POST")
			http.Error(w, "this endpoint accepts POST; drover sends no unsolicited messages", http.StatusMethodNotAllowed)
		case http.MethodDelete:
			// Sessions are not used, so there is nothing to tear down. Answer
			// cleanly rather than 405, since a client ending a session is
			// being well-behaved.
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "POST, DELETE")
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
		s.handleHTTPBatch(w, []byte(trimmed), r)
		return
	}

	var rpcReq request
	if err := json.Unmarshal([]byte(trimmed), &rpcReq); err != nil {
		writeRPC(w, http.StatusOK, &response{JSONRPC: "2.0", Error: errf(CodeParseError, "invalid JSON: %v", err)})
		return
	}

	resp := r.dispatch(&rpcReq)
	if resp == nil {
		// A notification gets no body. 202 is what the transport specifies.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

func (s *Server) handleHTTPBatch(w http.ResponseWriter, body []byte, r *router) {
	var reqs []request
	if err := json.Unmarshal(body, &reqs); err != nil {
		writeRPC(w, http.StatusOK, &response{JSONRPC: "2.0", Error: errf(CodeParseError, "invalid JSON: %v", err)})
		return
	}

	responses := make([]*response, 0, len(reqs))
	for i := range reqs {
		if resp := r.dispatch(&reqs[i]); resp != nil {
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

// originAllowed guards against a web page driving this endpoint.
//
// A request with no Origin is a direct client -- curl, an agent's HTTP client
// -- and is allowed. A request that carries one is a browser, and its origin
// must be a loopback address, which is the only place a legitimate local MCP
// client page could be served from.
func originAllowed(req *http.Request) bool {
	origin := req.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
