// Package client talks to a drover engine over HTTP.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/object"
)

// Client is a connection to one engine.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the engine at baseURL.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ErrNotFound is returned for a 404.
var ErrNotFound = errors.New("not found")

// serverError carries the engine's own message. It reports itself as
// ErrNotFound for a 404 without prefixing anything, because the server's
// message already says what was not found -- wrapping it produced
// "not found: Repository/nope: not found".
type serverError struct {
	msg      string
	notFound bool
}

func (e *serverError) Error() string { return e.msg }

func (e *serverError) Is(target error) bool {
	return target == ErrNotFound && e.notFound
}

func (c *Client) do(method, path string, body []byte, headers map[string]string, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the engine at %s: %w\nis `drover serve` running?", c.BaseURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return &serverError{msg: serverMessage(data), notFound: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &serverError{msg: serverMessage(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// serverMessage pulls the error text out of a response, falling back to the
// raw body so a proxy's HTML error is not swallowed into "unknown error".
func serverMessage(data []byte) string {
	var e api.Error
	if err := json.Unmarshal(data, &e); err == nil && e.Message != "" {
		return e.Message
	}
	msg := strings.TrimSpace(string(data))
	if msg == "" {
		return "the engine returned an error with no message"
	}
	return msg
}

// Status asks the engine what it is holding.
func (c *Client) Status() (*api.StatusResponse, error) {
	var out api.StatusResponse
	if err := c.do(http.MethodGet, api.Prefix+"/status", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Apply sends a whole batch. The server writes all of it or none.
func (c *Client) Apply(docs []api.Document) (*api.ApplyResponse, error) {
	body, err := json.Marshal(api.ApplyRequest{Documents: docs})
	if err != nil {
		return nil, err
	}
	var out api.ApplyResponse
	if err := c.do(http.MethodPost, api.Prefix+"/apply", body, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get fetches one object.
func (c *Client) Get(kind object.Kind, name string) (*api.ObjectView, error) {
	var out api.ObjectView
	if err := c.do(http.MethodGet, api.Prefix+"/"+kind.Plural()+"/"+name, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List fetches every object of one kind.
func (c *Client) List(kind object.Kind) ([]api.ObjectView, error) {
	var out api.ListResponse
	if err := c.do(http.MethodGet, api.Prefix+"/"+kind.Plural(), nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Sync forces a refresh of one repository.
func (c *Client) Sync(name string) error {
	return c.do(http.MethodPost, api.Prefix+"/repositories/"+name+"/sync", []byte("{}"), nil, nil)
}

// SyncAll forces a refresh of every repository.
func (c *Client) SyncAll() error {
	return c.do(http.MethodPost, api.Prefix+"/sync", []byte("{}"), nil, nil)
}

// Call executes one HTTPRequest.
func (c *Client) Call(name string, req api.CallRequest) (*api.CallResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out api.CallResponse
	if err := c.do(http.MethodPost, api.Prefix+"/httprequests/"+name+"/call", body, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Query runs one statement against a SQLConnection.
func (c *Client) Query(name, query string) (*api.QueryResponse, error) {
	body, err := json.Marshal(api.QueryRequest{Query: query})
	if err != nil {
		return nil, err
	}
	var out api.QueryResponse
	if err := c.do(http.MethodPost, api.Prefix+"/sqlconnections/"+name+"/query", body, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Health re-runs a SQLConnection's health gate.
func (c *Client) Health(name string) error {
	return c.do(http.MethodPost, api.Prefix+"/sqlconnections/"+name+"/health", []byte("{}"), nil, nil)
}

// Dashboard fetches everything the dashboard draws, in one round trip.
func (c *Client) Dashboard() (*api.DashboardResponse, error) {
	var out api.DashboardResponse
	if err := c.do(http.MethodGet, api.Prefix+"/dashboard", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reload asks the engine to re-read its config and apply it.
func (c *Client) Reload() (string, error) {
	var out api.ReloadResponse
	if err := c.do(http.MethodPost, api.Prefix+"/reload", []byte("{}"), nil, &out); err != nil {
		return "", err
	}
	return out.Message, nil
}

// --- file tools ---

// Ls lists a directory in the checkouts.
func (c *Client) Ls(req api.LsRequest) (*api.LsResponse, error) {
	var out api.LsResponse
	if err := c.post(api.Prefix+"/files/ls", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReadFile reads part of a file from the checkouts.
func (c *Client) ReadFile(req api.ReadRequest) (*api.ReadResponse, error) {
	var out api.ReadResponse
	if err := c.post(api.Prefix+"/files/read", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Grep searches file contents in the checkouts.
func (c *Client) Grep(req api.GrepRequest) (*api.GrepResponse, error) {
	var out api.GrepResponse
	if err := c.post(api.Prefix+"/files/grep", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Find matches paths in the checkouts.
func (c *Client) Find(req api.FindRequest) (*api.FindResponse, error) {
	var out api.FindResponse
	if err := c.post(api.Prefix+"/files/find", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// post marshals a body, sends it, and decodes the reply.
func (c *Client) post(path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(http.MethodPost, path, data, nil, out)
}

// Delete removes one object and, for a Repository, its checkout.
func (c *Client) Delete(kind object.Kind, name string) error {
	return c.do(http.MethodDelete, api.Prefix+"/"+kind.Plural()+"/"+name, nil, nil, nil)
}
