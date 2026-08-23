// Package mcp serves drover's objects to coding agents over the Model Context
// Protocol.
//
// The transport is JSON-RPC 2.0 over stdio, which is what agents spawn. It is
// hand-rolled rather than pulled from an SDK: the surface drover needs is
// initialize, tools/list and tools/call, and owning those few hundred lines is
// cheaper than owning a dependency.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC error codes from the spec, plus the ones MCP leans on.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// request is an incoming JSON-RPC message.
//
// ID is json.RawMessage because the spec allows a string or a number and
// echoing back exactly what arrived is simpler, and safer, than guessing.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the peer expects no reply. A notification
// carries no id, and replying to one is a protocol violation.
func (r *request) isNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

func errf(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// handler is one JSON-RPC method.
//
// The context belongs to the one message being answered, not to the
// connection: over HTTP it is the request's, so a client that hangs up
// cancels the tool call it was waiting on.
type handler func(ctx context.Context, params json.RawMessage) (any, *rpcError)

// router maps methods to handlers. It is transport-agnostic on purpose: the
// stdio connection and the HTTP endpoint dispatch through the same table, so
// the two transports cannot drift in what they support.
type router struct {
	handlers map[string]handler
}

func newRouter() *router { return &router{handlers: map[string]handler{}} }

func (r *router) handle(method string, h handler) { r.handlers[method] = h }

// dispatch runs one message. It returns nil when there is nothing to send
// back, which is the case for a notification.
func (r *router) dispatch(ctx context.Context, req *request) *response {
	h, ok := r.handlers[req.Method]
	if !ok {
		if req.isNotification() {
			// Unknown notifications are ignored by design: the protocol grows
			// new ones, and an old server should not break on them.
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Error: errf(CodeMethodNotFound, "unknown method %q", req.Method)}
	}

	result, rpcErr := h(ctx, req.Params)
	if req.isNotification() {
		return nil
	}
	if rpcErr != nil {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

// conn is a JSON-RPC connection over a reader and a writer.
type conn struct {
	in  *bufio.Reader
	out io.Writer

	mu     sync.Mutex // serialises writes; one message per line
	router *router
}

func newConn(in io.Reader, out io.Writer, r *router) *conn {
	return &conn{
		in:     bufio.NewReaderSize(in, 1<<20),
		out:    out,
		router: r,
	}
}

// serve reads messages until the input closes, or the context is cancelled.
//
// Messages are answered one at a time, so the context handed to each is the
// connection's: there is no second message waiting to be served while one is
// in flight, and nothing to cancel a call individually with.
//
// Messages are newline-delimited JSON, which is what the stdio transport
// specifies. A single message may be large -- a read of a big file comes back
// this way -- so the reader is given room and long lines are accumulated
// rather than truncated.
func (c *conn) serve(ctx context.Context) error {
	for {
		line, err := c.readLine()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			// There is no id to answer with, so this is the one case where a
			// reply carries a null id, as the spec requires.
			c.write(&response{JSONRPC: "2.0", Error: errf(CodeParseError, "invalid JSON: %v", err)})
			continue
		}
		if resp := c.router.dispatch(ctx, &req); resp != nil {
			c.write(resp)
		}
	}
}

// readLine reads one newline-delimited message, joining the pieces bufio
// hands back when a message is longer than the buffer.
func (c *conn) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := c.in.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}

// notify sends a server-initiated message. It carries no id, so the peer must
// not answer it -- which is also why it goes out through the same mutex as
// every reply: two goroutines interleaving bytes on one line would corrupt
// both messages.
func (c *conn) notify(method string, params any) {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.out.Write(append(data, '\n'))
}

func (c *conn) write(resp *response) {
	data, err := json.Marshal(resp)
	if err != nil {
		data, _ = json.Marshal(&response{
			JSONRPC: "2.0",
			ID:      resp.ID,
			Error:   errf(CodeInternalError, "could not encode the result: %v", err),
		})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.out.Write(append(data, '\n'))
}
