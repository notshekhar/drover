package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultRequestTimeout caps one request. A server that is still indexing can
// sit on a request indefinitely, and a tool call that never returns is worse
// than one that says "still indexing".
const DefaultRequestTimeout = 20 * time.Second

// stderrLines is how much of a server's complaining we keep. It is the only
// evidence available when a server dies during the handshake, which is the
// failure that otherwise looks like "this language is not supported".
const stderrLines = 40

// Client is one language-server process.
//
// A single goroutine owns reading. Everything else hands it a channel and
// waits, so there is no lock held across a request and a slow server cannot
// block an unrelated one.
type Client struct {
	Key  string // registry key, e.g. "go"
	Root string // absolute project root this server was started for

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *ring

	mu      sync.Mutex
	nextID  int
	pending map[int]chan *rpcResponse
	opened  map[string]int // uri -> version
	closed  bool

	capsMu sync.RWMutex
	caps   map[string]json.RawMessage

	diagMu sync.Mutex
	diags  map[string][]Diagnostic

	// progress counts the work-done tokens the server has open. A server that
	// is indexing says so this way, and answering "still indexing" beats
	// answering "no results found" from an index that is half built.
	progressMu sync.Mutex
	progress   map[string]string

	done    chan struct{}
	exitErr error
}

type rpcResponse struct {
	Result json.RawMessage
	Err    *rpcError
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// message is a JSON-RPC frame in either direction. Which fields are present is
// what says whether it is a request, a response or a notification.
type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

// Start launches a server process and begins reading from it. It does not
// hand-shake; call Initialize next.
func Start(ctx context.Context, key, root, bin string, args []string, env []string) (*Client, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	c := &Client{
		Key:      key,
		Root:     root,
		cmd:      cmd,
		stdin:    stdin,
		stderr:   newRing(stderrLines),
		pending:  map[int]chan *rpcResponse{},
		opened:   map[string]int{},
		caps:     map[string]json.RawMessage{},
		diags:    map[string][]Diagnostic{},
		progress: map[string]string{},
		done:     make(chan struct{}),
	}
	go c.readStderr(errPipe)
	go c.read(stdout)
	return c, nil
}

// Alive reports whether the process is still running.
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Stderr returns what the server last complained about.
func (c *Client) Stderr() string { return c.stderr.String() }

// --- reading ---

func (c *Client) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8<<10), 1<<20)
	for sc.Scan() {
		c.stderr.add(sc.Text())
	}
}

// read owns the response stream until the process goes away.
func (c *Client) read(r io.Reader) {
	defer c.shutdownPending()

	br := bufio.NewReaderSize(r, 64<<10)
	for {
		body, err := readFrame(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.exitErr = err
			}
			return
		}
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			continue // a frame we cannot parse is not a reason to stop reading
		}
		c.dispatch(&m)
	}
}

// readFrame reads one Content-Length framed message.
//
// Anything before the first header that is not a header is skipped rather than
// treated as a protocol error: several servers print a banner or a warning on
// stdout before they start speaking, and dying on it would make them look
// broken.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if length < 0 {
				continue // a blank line before any header is noise
			}
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", value)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("a message arrived with no Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *Client) dispatch(m *message) {
	switch {
	case m.ID != nil && m.Method == "":
		c.deliver(m)
	case m.ID != nil:
		// A request FROM the server. Every one of these must be answered:
		// gopls asks for configuration during initialization and waits, so a
		// client that ignores server requests hangs on its first real call.
		c.answer(m)
	default:
		c.notified(m)
	}
}

func (c *Client) deliver(m *message) {
	var id int
	if err := json.Unmarshal(*m.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		return
	}
	ch <- &rpcResponse{Result: m.Result, Err: m.Error}
}

// answer replies to a server-initiated request.
//
// The replies are deliberately empty: drover has no settings to hand a server,
// no UI to show a progress bar in, and nothing to refresh. Saying so promptly
// is the whole job -- what matters is that the server is never left waiting.
func (c *Client) answer(m *message) {
	var result any = nil
	switch m.Method {
	case "workspace/configuration":
		// One entry per requested section, or the server indexes into a
		// shorter array than it asked for.
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(m.Params, &params)
		items := make([]map[string]any, len(params.Items))
		for i := range items {
			items[i] = map[string]any{}
		}
		result = items
	case "workspace/workspaceFolders":
		result = []map[string]string{{"uri": PathToURI(c.Root), "name": c.Key}}
	}
	c.send(message{JSONRPC: "2.0", ID: m.ID, Result: mustJSON(result)})
}

func (c *Client) notified(m *message) {
	switch m.Method {
	case "textDocument/publishDiagnostics":
		var params PublishDiagnosticsParams
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return
		}
		c.diagMu.Lock()
		c.diags[params.URI] = params.Diagnostics
		c.diagMu.Unlock()

	case "$/progress":
		c.trackProgress(m.Params)
	}
}

// trackProgress follows work-done tokens so Indexing can answer honestly.
func (c *Client) trackProgress(raw json.RawMessage) {
	var params struct {
		Token any `json:"token"`
		Value struct {
			Kind    string `json:"kind"`
			Title   string `json:"title"`
			Message string `json:"message"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	token := fmt.Sprint(params.Token)

	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	switch params.Value.Kind {
	case "begin":
		c.progress[token] = strings.TrimSpace(params.Value.Title + " " + params.Value.Message)
	case "report":
		if params.Value.Message != "" {
			c.progress[token] = strings.TrimSpace(params.Value.Title + " " + params.Value.Message)
		}
	case "end":
		delete(c.progress, token)
	}
}

// Indexing reports what the server is busy with, or "" when it is idle.
func (c *Client) Indexing() string {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	for _, what := range c.progress {
		if what != "" {
			return what
		}
	}
	return ""
}

func (c *Client) shutdownPending() {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int]chan *rpcResponse{}
	c.closed = true
	c.mu.Unlock()

	err := &rpcError{Message: "the language server exited"}
	if last := c.stderr.String(); last != "" {
		err.Message += ": " + lastLine(last)
	}
	for _, ch := range pending {
		ch <- &rpcResponse{Err: err}
	}
	close(c.done)
	_ = c.cmd.Wait()
}

// --- writing ---

func (c *Client) send(m message) {
	m.JSONRPC = "2.0"
	body, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body))
	_, _ = c.stdin.Write(body)
}

// Request sends a request and waits for its response.
func (c *Client) Request(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("the language server is not running")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	rawID := json.RawMessage(strconv.Itoa(id))
	c.send(message{ID: &rawID, Method: method, Params: mustJSON(params)})

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		if what := c.Indexing(); what != "" {
			return fmt.Errorf("the %s server is still indexing (%s); ask again in a moment", c.Key, what)
		}
		return fmt.Errorf("the %s server did not answer %s in time", c.Key, method)
	case resp := <-ch:
		if resp.Err != nil {
			return fmt.Errorf("%s: %s", method, resp.Err.Message)
		}
		if out == nil || len(resp.Result) == 0 || string(resp.Result) == "null" {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}
}

// Notify sends a notification, which has no reply.
func (c *Client) Notify(method string, params any) {
	c.send(message{Method: method, Params: mustJSON(params)})
}

// --- lifecycle ---

// Initialize performs the handshake and records the server's capabilities.
func (c *Client) Initialize(ctx context.Context, initOptions any) error {
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   PathToURI(c.Root),
		"rootPath":  c.Root,
		"clientInfo": map[string]string{
			"name":    "drover",
			"version": "1",
		},
		"workspaceFolders": []map[string]string{
			{"uri": PathToURI(c.Root), "name": c.Key},
		},
		"capabilities": map[string]any{
			"workspace": map[string]any{
				"workspaceFolders": true,
				"configuration":    true,
				"symbol":           map[string]any{"dynamicRegistration": false},
			},
			"textDocument": map[string]any{
				"synchronization": map[string]any{"didSave": false, "willSave": false},
				// linkSupport asks for LocationLink, which carries the
				// selection range: the name, rather than the whole body of
				// whatever was defined.
				"definition":         map[string]any{"linkSupport": true},
				"implementation":     map[string]any{"linkSupport": true},
				"typeDefinition":     map[string]any{"linkSupport": true},
				"references":         map[string]any{},
				"hover":              map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"callHierarchy":      map[string]any{},
				"publishDiagnostics": map[string]any{},
				"diagnostic":         map[string]any{"dynamicRegistration": false},
			},
			"window": map[string]any{"workDoneProgress": true},
		},
	}
	if initOptions != nil {
		params["initializationOptions"] = initOptions
	}

	var result struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := c.Request(ctx, "initialize", params, &result); err != nil {
		return err
	}
	c.capsMu.Lock()
	c.caps = result.Capabilities
	c.capsMu.Unlock()

	c.Notify("initialized", map[string]any{})
	return nil
}

// Supports reports whether the server advertised a capability.
//
// A capability may be `true` or an options object, and either means yes;
// `false` and absent both mean no.
func (c *Client) Supports(capability string) bool {
	c.capsMu.RLock()
	raw, ok := c.caps[capability]
	c.capsMu.RUnlock()
	if !ok {
		return false
	}
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "false" && s != "null"
}

// OpenDocument sends didOpen the first time and didChange after.
//
// drover never edits, so the content is always what is on disk. It still has
// to be sent: a server builds its view of a project from the documents it has
// been given, and asking about a file it has never seen is how you get an
// empty answer from a working server.
func (c *Client) OpenDocument(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	uri := PathToURI(path)

	c.mu.Lock()
	version, seen := c.opened[uri]
	version++
	c.opened[uri] = version
	c.mu.Unlock()

	if !seen {
		c.Notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": LanguageID(path),
				"version":    version,
				"text":       string(content),
			},
		})
		return nil
	}
	c.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": string(content)}},
	})
	return nil
}

// Diagnostics returns what the server has said about a file.
//
// LSP has two mechanisms and a client that implements one of them gets silence
// from servers that use the other. The push model volunteers
// publishDiagnostics; the pull model answers textDocument/diagnostic only when
// asked. Ask when the capability is there, and merge with anything pushed --
// waiting for a push from a pull-only server just burns the timeout and
// reports a clean file.
func (c *Client) Diagnostics(ctx context.Context, path string, wait time.Duration) []Diagnostic {
	uri := PathToURI(path)

	var pulled []Diagnostic
	if c.Supports("diagnosticProvider") {
		var report struct {
			Items []Diagnostic `json:"items"`
		}
		if err := c.Request(ctx, "textDocument/diagnostic", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		}, &report); err == nil {
			pulled = report.Items
		}
	}

	// Give a push-model server a moment to volunteer something before
	// concluding the file is clean.
	deadline := time.Now().Add(wait)
	for {
		c.diagMu.Lock()
		pushed, ok := c.diags[uri]
		c.diagMu.Unlock()
		if ok || time.Now().After(deadline) {
			return dedupe(append(pulled, pushed...))
		}
		select {
		case <-c.done:
			return dedupe(pulled)
		case <-ctx.Done():
			return dedupe(pulled)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func dedupe(in []Diagnostic) []Diagnostic {
	if len(in) < 2 {
		return in
	}
	seen := map[string]bool{}
	out := in[:0]
	for _, d := range in {
		key := fmt.Sprintf("%d:%d:%s", d.Range.Start.Line, d.Range.Start.Character, d.Message)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// Close asks the server to stop, then makes sure it did.
//
// The polite sequence is shutdown then exit. A server that ignores it gets
// killed: a language server that outlives the engine is a gigabyte of resident
// memory nobody owns.
func (c *Client) Close() {
	if !c.Alive() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.Request(ctx, "shutdown", nil, nil)
	cancel()
	c.Notify("exit", nil)

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
	_ = c.stdin.Close()
}

// --- helpers ---

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// ring keeps the last n lines and forgets the rest.
type ring struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}
