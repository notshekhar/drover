// Package api is the wire contract between the drover client and server.
//
// It is deliberately small and shared, so the two sides cannot drift.
package api

// Version is the API group and version in route paths.
const Version = "drover/v1"

// Prefix is the route prefix: /apis/drover/v1.
const Prefix = "/apis/" + Version

// Document is one raw YAML document plus where it came from. The client sends
// the text rather than a parsed object so the server does its own validation
// -- an API that trusts its client is not an API.
type Document struct {
	Source string `json:"source"`
	Data   string `json:"data"`
}

// ApplyRequest is a whole apply invocation. The server validates every
// document and writes either all of them or none.
type ApplyRequest struct {
	Documents []Document `json:"documents"`
}

// Action is what an apply did to one object.
type Action string

const (
	ActionCreated   Action = "created"
	ActionUpdated   Action = "updated"
	ActionUnchanged Action = "unchanged"
)

// Result is what happened to one object.
type Result struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action Action `json:"action"`
}

// ApplyResponse reports the outcome of a successful apply.
type ApplyResponse struct {
	Results  []Result `json:"results"`
	Warnings []string `json:"warnings,omitempty"`
}

// ObjectView is one object as the server reports it.
type ObjectView struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Source    string `json:"source,omitempty"`
	AppliedAt string `json:"appliedAt,omitempty"`

	// YAML is the stored document, so `drover get -o yaml` can print exactly
	// what the engine holds.
	YAML string `json:"yaml,omitempty"`

	// Repository fields, set when Kind is Repository.
	URL             string `json:"url,omitempty"`
	Branch          string `json:"branch,omitempty"`
	RefreshInterval string `json:"refreshInterval,omitempty"`

	// Observed state, from the last reconcile or health check.
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
	Commit   string `json:"commit,omitempty"`
	LastSync string `json:"lastSync,omitempty"`

	// Environment fields.
	Variables []string       `json:"variables,omitempty"`
	Secrets   []SecretStatus `json:"secrets,omitempty"`

	// HTTPRequest fields.
	Method             string   `json:"method,omitempty"`
	Environments       []string `json:"environments,omitempty"`
	DefaultEnvironment string   `json:"defaultEnvironment,omitempty"`
	Safe               bool     `json:"safe,omitempty"`
	Params             []string `json:"params,omitempty"`

	// SQLConnection fields.
	Provider string `json:"provider,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	MaxRows  int    `json:"maxRows,omitempty"`
}

// SecretStatus describes a secret without revealing it.
type SecretStatus struct {
	Name    string `json:"name"`
	FromEnv string `json:"fromEnv"`
	Set     bool   `json:"set"`
}

// ListResponse is a list of objects.
type ListResponse struct {
	Items []ObjectView `json:"items"`
}

// Error is the body of any non-2xx response.
type Error struct {
	Message string `json:"error"`
}

// CallRequest executes one HTTPRequest object.
type CallRequest struct {
	Environment string            `json:"environment,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
	// AllowUnsafeMethod permits a non-GET request. MCP never sets it.
	AllowUnsafeMethod bool `json:"allowUnsafeMethod,omitempty"`
}

// CallResponse is what the remote returned.
type CallResponse struct {
	Status     int               `json:"status"`
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"`
	DurationMS int64             `json:"durationMs"`
}

// QueryRequest runs one statement against a SQLConnection.
type QueryRequest struct {
	Query string `json:"query"`
}

// QueryResponse is a result set.
type QueryResponse struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	RowCount  int        `json:"rowCount"`
	Truncated bool       `json:"truncated,omitempty"`
	Provider  string     `json:"provider"`
	ElapsedMS int64      `json:"elapsedMs"`
}

// --- file tools ---

// LsRequest lists a directory under the repository root.
type LsRequest struct {
	Path string `json:"path,omitempty"`
}

// FileEntry is one directory entry.
type FileEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

// LsResponse is a directory listing.
type LsResponse struct {
	Path      string      `json:"path"`
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
}

// ReadRequest reads part of a file. Offset is a 1-based line number.
type ReadRequest struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ReadResponse is file content.
type ReadResponse struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"startLine"`
	EndLine    int    `json:"endLine"`
	TotalLines int    `json:"totalLines"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// GrepRequest searches file contents.
type GrepRequest struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path,omitempty"`
	Include       string `json:"include,omitempty"`
	CaseSensitive bool   `json:"caseSensitive,omitempty"`
	MaxResults    int    `json:"maxResults,omitempty"`
}

// GrepMatch is one hit.
type GrepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepResponse is a set of hits.
type GrepResponse struct {
	Matches   []GrepMatch `json:"matches"`
	Files     int         `json:"filesSearched"`
	Truncated bool        `json:"truncated,omitempty"`
}

// FindRequest matches paths by glob.
type FindRequest struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	MaxResults int    `json:"maxResults,omitempty"`
}

// FindResponse is a set of paths.
type FindResponse struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`
}

// ReloadResponse reports what a reload did.
type ReloadResponse struct {
	Message string `json:"message"`
}

// DashboardResponse is everything the dashboard draws, in one round trip.
//
// It is its own endpoint rather than four list calls because the dashboard
// repaints on a timer, and four requests a second against a remote engine is
// wasteful when one will do.
type DashboardResponse struct {
	Version   string        `json:"version"`
	DataDir   string        `json:"dataDir"`
	Listen    string        `json:"listen"`
	UptimeSec int64         `json:"uptimeSec"`
	Repos     []DashRepo    `json:"repos"`
	Requests  []DashRequest `json:"requests"`
	SQLs      []DashSQL     `json:"sqls"`
	Envs      []DashEnv     `json:"envs"`
}

// DashRepo is one repository row.
type DashRepo struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	Refresh  string `json:"refresh"`
	Status   string `json:"status"`
	Commit   string `json:"commit,omitempty"`
	LastSync string `json:"lastSync,omitempty"`
	Error    string `json:"error,omitempty"`
}

// DashRequest is one HTTPRequest row.
type DashRequest struct {
	Name        string `json:"name"`
	Method      string `json:"method"`
	URL         string `json:"url"`
	Environment string `json:"environment"`
	Offered     bool   `json:"offered"`
}

// DashSQL is one SQLConnection row.
type DashSQL struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	ReadOnly bool   `json:"readOnly"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// DashEnv is one Environment row.
type DashEnv struct {
	Name      string `json:"name"`
	Variables int    `json:"variables"`
	Secrets   int    `json:"secrets"`
	Unset     int    `json:"unset"`
}

// StatusResponse answers "is the engine up and what is it holding".
type StatusResponse struct {
	Version string `json:"version"`
	DataDir string `json:"dataDir"`
	Objects int    `json:"objects"`
}
