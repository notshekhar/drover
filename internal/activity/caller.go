package activity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// NewSession returns an 8-character id for one MCP connection.
func NewSession() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Header names the stdio bridge uses to tell the engine who is calling.
// Nothing in them is trusted for authorisation; they label a log line.
const (
	HeaderClient  = "X-Drover-Client"
	HeaderSession = "X-Drover-Session"
	HeaderVia     = "X-Drover-Via"
)

// Caller is who made a tool call, as far as drover can measure.
type Caller struct {
	Source  string // mcp-http | mcp-stdio | cli | web
	Client  string // "claude-code/2.1.4"
	Session string
}

type callerKey struct{}

// WithCaller attaches attribution to a context.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom reads attribution off a context. Missing is a zero value.
func CallerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// CallerFromHTTP reads the stdio-bridge headers, or defaults to cli.
func CallerFromHTTP(r *http.Request) Caller {
	via := strings.TrimSpace(r.Header.Get(HeaderVia))
	c := Caller{
		Client:  strings.TrimSpace(r.Header.Get(HeaderClient)),
		Session: strings.TrimSpace(r.Header.Get(HeaderSession)),
	}
	switch via {
	case "stdio":
		c.Source = "mcp-stdio"
	case "web":
		c.Source = "web"
	case "mcp-http":
		c.Source = "mcp-http"
	default:
		if via != "" {
			c.Source = via
		} else {
			c.Source = "cli"
		}
	}
	return c
}

// SetHeaders writes attribution onto an outgoing request to the engine.
func (c Caller) SetHeaders(h http.Header) {
	if c.Client != "" {
		h.Set(HeaderClient, c.Client)
	}
	if c.Session != "" {
		h.Set(HeaderSession, c.Session)
	}
	if c.Source == "mcp-stdio" {
		h.Set(HeaderVia, "stdio")
	} else if c.Source != "" && c.Source != "cli" {
		h.Set(HeaderVia, c.Source)
	}
}
