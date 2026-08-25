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

	// AllowInlinePasswords accepts a password inside a SQLConnection url.
	// Off by default: a credential in a file people commit is a leak.
	AllowInlinePasswords bool `json:"allowInlinePasswords,omitempty"`
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
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Source    string            `json:"source,omitempty"`
	AppliedAt string            `json:"appliedAt,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`

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

	// Mirror is what the discussion mirror last did, and MirrorError why it
	// could not. They are separate from Status and Error because a mirror
	// that cannot reach GitHub is not a checkout that failed.
	Mirror      string `json:"mirror,omitempty"`
	MirrorError string `json:"mirrorError,omitempty"`

	// Schema is where a SQLConnection's dumped shape landed.
	Schema      string `json:"schema,omitempty"`
	SchemaError string `json:"schemaError,omitempty"`

	// Config is what a repository's own .drover.yaml contributed.
	Config      string `json:"config,omitempty"`
	ConfigError string `json:"configError,omitempty"`

	// DocumentStore fields, set when Kind is DocumentStore.
	Description string `json:"description,omitempty"`
	Writable    bool   `json:"writable,omitempty"`
	Documents   int    `json:"documents,omitempty"`

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

	// Root marks a top-level entry that is a store of something other than
	// source -- mirrored issues, documents -- rather than a checkout.
	Root bool `json:"root,omitempty"`
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

	// Selector narrows the search to the repositories whose labels match,
	// instead of to a path. On a warehouse of any size this is the difference
	// between a search and a scan.
	Selector string `json:"selector,omitempty"`
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

	// Unsearched names the roots a search with no path did not walk.
	Unsearched []string `json:"unsearched,omitempty"`
}

// FindRequest matches paths by glob.
type FindRequest struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	MaxResults int    `json:"maxResults,omitempty"`
	Selector   string `json:"selector,omitempty"`
}

// FindResponse is a set of paths.
type FindResponse struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`

	// Unsearched names the roots a search with no path did not walk.
	Unsearched []string `json:"unsearched,omitempty"`
}

// DocWriteRequest writes one document into a store.
type DocWriteRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Reason  string `json:"reason,omitempty"`
}

// DocWriteResponse is what a write did.
type DocWriteResponse struct {
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	Created   bool   `json:"created"`
	Commit    string `json:"commit,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
}

// DocumentStoreView is one store, for a listing.
type DocumentStoreView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
	Writable    bool   `json:"writable"`
	Documents   int    `json:"documents"`
}

// Hotspot is one path agents keep coming back to, and EmptySearch a pattern
// they keep failing to find.
type Hotspot struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Reads      int    `json:"reads"`
}

// EmptySearch is a pattern searched for repeatedly with no result.
type EmptySearch struct {
	Pattern string `json:"pattern"`
	Times   int    `json:"times"`
}

// HotspotsResponse is what the activity ledger knows about where agents go.
type HotspotsResponse struct {
	Hotspots []Hotspot     `json:"hotspots"`
	Empty    []EmptySearch `json:"emptySearches,omitempty"`
}

// ReviewResponse is what a repository's own .drover.yaml declared, and
// whether any of it was applied.
type ReviewResponse struct {
	Repository string `json:"repository"`
	Trusted    bool   `json:"trusted"`
	Documents  string `json:"documents,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Error      string `json:"error,omitempty"`
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

	// Mirror is what the discussion mirror last did, and MirrorError why it
	// could not. They are separate from Status and Error because a mirror
	// that cannot reach GitHub is not a checkout that failed.
	Mirror      string `json:"mirror,omitempty"`
	MirrorError string `json:"mirrorError,omitempty"`

	// Schema is where a SQLConnection's dumped shape landed.
	Schema      string `json:"schema,omitempty"`
	SchemaError string `json:"schemaError,omitempty"`

	// Config is what a repository's own .drover.yaml contributed.
	Config      string `json:"config,omitempty"`
	ConfigError string `json:"configError,omitempty"`

	// DocumentStore fields, set when Kind is DocumentStore.
	Description string `json:"description,omitempty"`
	Writable    bool   `json:"writable,omitempty"`
	Documents   int    `json:"documents,omitempty"`
	Error       string `json:"error,omitempty"`
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

// --- git tool ---

// GitRequest is one history question. Which fields matter depends on
// Operation; the engine says so plainly when a required one is missing.
type GitRequest struct {
	Operation  string `json:"operation"`
	Repository string `json:"repository,omitempty"`
	Path       string `json:"path,omitempty"`
	Rev        string `json:"rev,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Author     string `json:"author,omitempty"`
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	Grep       string `json:"grep,omitempty"`
	Query      string `json:"query,omitempty"`
	Regex      bool   `json:"regex,omitempty"`
	Merges     string `json:"merges,omitempty"`
	Patch      bool   `json:"patch,omitempty"`
	Lines      string `json:"lines,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// GitCommit is one commit's metadata.
type GitCommit struct {
	Hash       string   `json:"hash"`
	Short      string   `json:"short"`
	Author     string   `json:"author"`
	Email      string   `json:"email,omitempty"`
	Date       string   `json:"date"`
	Committer  string   `json:"committer,omitempty"`
	CommitDate string   `json:"commitDate,omitempty"`
	Parents    []string `json:"parents,omitempty"`
	Subject    string   `json:"subject"`
	Body       string   `json:"body,omitempty"`
	Files      int      `json:"files,omitempty"`
	Insertions int      `json:"insertions,omitempty"`
	Deletions  int      `json:"deletions,omitempty"`
}

// GitFileChange is one path a commit or a diff touched.
type GitFileChange struct {
	Status     string `json:"status"`
	Path       string `json:"path"`
	OldPath    string `json:"oldPath,omitempty"`
	Insertions int    `json:"insertions,omitempty"`
	Deletions  int    `json:"deletions,omitempty"`
	Binary     bool   `json:"binary,omitempty"`
}

// GitBlameLine is one line with the commit that last touched it.
type GitBlameLine struct {
	Line    int    `json:"line"`
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Summary string `json:"summary,omitempty"`
	Text    string `json:"text"`
}

// GitRef is a branch or a tag.
type GitRef struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Date    string `json:"date,omitempty"`
	Subject string `json:"subject,omitempty"`
	Head    bool   `json:"head,omitempty"`
}

// GitAuthor is one contributor's tally.
type GitAuthor struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Commits int    `json:"commits"`
	First   string `json:"first,omitempty"`
	Last    string `json:"last,omitempty"`
}

// GitStatus is the state of one checkout.
type GitStatus struct {
	Repository string    `json:"repository"`
	URL        string    `json:"url,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Head       GitCommit `json:"head"`
	Commits    int       `json:"commits,omitempty"`
	FirstDate  string    `json:"firstDate,omitempty"`
	Clean      bool      `json:"clean"`
	Dirty      []string  `json:"dirty,omitempty"`
	LastFetch  string    `json:"lastFetch,omitempty"`
}

// GitResponse is a union: which fields are set depends on the operation.
type GitResponse struct {
	Operation  string `json:"operation"`
	Repository string `json:"repository"`
	Branch     string `json:"branch,omitempty"`
	Range      string `json:"range,omitempty"`
	Rev        string `json:"rev,omitempty"`
	Path       string `json:"path,omitempty"`

	Commits []GitCommit     `json:"commits,omitempty"`
	Commit  *GitCommit      `json:"commit,omitempty"`
	Files   []GitFileChange `json:"files,omitempty"`
	Patch   string          `json:"patch,omitempty"`
	Blame   []GitBlameLine  `json:"blame,omitempty"`
	Refs    []GitRef        `json:"refs,omitempty"`
	Authors []GitAuthor     `json:"authors,omitempty"`
	Status  *GitStatus      `json:"status,omitempty"`
	Content string          `json:"content,omitempty"`

	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// --- lsp tool ---

// LSPRequest is one navigation question.
type LSPRequest struct {
	Operation  string `json:"operation"`
	Path       string `json:"path,omitempty"`
	Line       int    `json:"line,omitempty"`
	Character  int    `json:"character,omitempty"`
	Symbol     string `json:"symbol,omitempty"`
	Occurrence int    `json:"occurrence,omitempty"`
	Query      string `json:"query,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// LSPRef is a place in the code.
type LSPRef struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Text      string `json:"text,omitempty"`
}

// LSPSymbol is one entry of an outline or a symbol search.
type LSPSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Depth     int    `json:"depth,omitempty"`
}

// LSPProblem is one diagnostic.
type LSPProblem struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Severity  string `json:"severity"`
	Source    string `json:"source,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
}

// LSPCall is one edge of a call graph.
type LSPCall struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Line  int    `json:"line"`
	Sites []int  `json:"sites,omitempty"`
}

// LSPServer is one language server, running or merely possible.
type LSPServer struct {
	Key       string `json:"key"`
	Language  string `json:"language"`
	State     string `json:"state"`
	Repo      string `json:"repository,omitempty"`
	Root      string `json:"root,omitempty"`
	Bin       string `json:"bin,omitempty"`
	Source    string `json:"source,omitempty"`
	Version   string `json:"version,omitempty"`
	UptimeSec int64  `json:"uptimeSec,omitempty"`
	IdleSec   int64  `json:"idleSec,omitempty"`
	Requests  int    `json:"requests,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// LSPResponse is a union: which fields are set depends on the operation.
type LSPResponse struct {
	Operation string `json:"operation"`
	Path      string `json:"path,omitempty"`
	Language  string `json:"language,omitempty"`
	Position  string `json:"position,omitempty"`

	Refs     []LSPRef     `json:"refs,omitempty"`
	Hover    string       `json:"hover,omitempty"`
	Symbols  []LSPSymbol  `json:"symbols,omitempty"`
	Problems []LSPProblem `json:"problems,omitempty"`
	Calls    []LSPCall    `json:"calls,omitempty"`
	Servers  []LSPServer  `json:"servers,omitempty"`

	Truncated   bool   `json:"truncated,omitempty"`
	Note        string `json:"note,omitempty"`
	ServerState string `json:"serverState,omitempty"`
}
