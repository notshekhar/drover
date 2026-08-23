# drover — plan: activity tracking and the web dashboard

**Status: phases 1, 2, 7 and 8 are built** (see the build order below); the
rest is not. Three things, in dependency order:

1. **Activity** — every tool call recorded: what ran, for whom, how long, what
   it touched, what came back, and why it ran.
2. **A web dashboard** at `/dashboard/...` — a page per object, plus the full
   activity view.
3. **The seams both force** — a `context.Context` through MCP, one execution
   path instead of two, and one server-push channel.

The third is the reason this is worth doing carefully. drover currently has no
way to cancel a tool call, no way to know who made one, and two independent
implementations of every tool. Activity cannot be recorded honestly until
those are fixed, so this plan fixes them first and gets the feature as a
consequence.

---

## 1. What "why" can honestly mean

The ask is to track *why* a tool ran. Most of that is not knowable, and saying
so up front decides the design.

MCP carries no intent. A `tools/call` is a name and an arguments object.
There is no field for the model's reason, no field for the user's prompt, and
no field for the turn it belongs to. Anything drover claims about intent it
either measured or invented.

Three things it can measure, in descending honesty:

**Attribution — who.** `initialize` carries `clientInfo` (name, version) and
the transport tells us the rest. So: `claude-code 2.1.4 over stdio`, or
`cursor over http`, or `cli`, or `web`. This is real and currently thrown
away.

**The chain — what came before.** Tool calls arrive in bursts inside one
session. `grep authenticateUser` → `read auth/session.go` → `git blame` on the
line it found is a legible act of reasoning, and it is legible *only* as a
sequence. Recording the session id, the position in it, and the gap since the
previous call turns a flat log into something you can read. This is the
closest honest thing to intent, and it costs nothing but a counter.

**The stated reason — what the model says it is doing.** Add an optional
`reason` argument to every tool:

```
"reason": {
  "type": "string",
  "description": "One short line on what you are trying to find out. Shown to
                  the human running this engine; it is never used to answer."
}
```

Models fill these in reliably — it is the same trick as the `description`
argument on Claude Code's own Bash tool. It is **self-reported**, so it is
recorded as a claim, rendered in quotes, and never treated as fact. Behind
`activity.askWhy` (default on) so anyone who resents the tokens can turn it
off.

What is **not** recorded, ever: a guess at intent inferred from arguments. A
log that says "looking for auth code" because the pattern contained `auth` is
a log that lies at exactly the moment you need it.

---

## 2. The recording seam

### The problem

There are two independent paths into every tool, and they share no code:

| caller | path |
|---|---|
| HTTP `/mcp` | `mcp.Server` → `server.backend` (in-process) → executors |
| `drover mcp` (stdio) | `mcp.Server` → `client` → HTTP → `server.handleGrep` → executors |
| `drover call` / `query` | `client` → HTTP → `server.handleCall` → executors |

`internal/server/backend.go` and the REST handlers in
`internal/server/server.go` are parallel implementations of the same nine
operations. `backend.Grep` and `handleGrep` (`server.go:776`) both call
`s.files.Grep` and both build an `api.GrepResponse` by hand. Recording in one
misses the other; recording in both double-counts the stdio path, which
crosses both.

### The fix, in three moves

**Move 1 — thread `context.Context` through MCP.** There is not one `context`
in `internal/mcp` today. `handler` is `func(params json.RawMessage) (any,
*rpcError)` (`jsonrpc.go:63`), so `dispatch` has nothing to pass down. Change
it to take a `ctx` and thread it from both transports.

This is worth doing on its own merits, independent of activity: right now a
`grep` across forty repositories keeps running after the client hangs up,
because nothing can cancel it. The context is also the carrier for
attribution, so one change buys cancellation, deadlines, and the *who*.

**Move 2 — make the REST handlers thin adapters over `backend`.** Delete the
duplicated bodies; `handleGrep` decodes, calls `b.Grep`, writes. The response
shaping lives in one place. This removes ~200 lines and, more importantly,
makes a single wrapper able to see everything.

**Move 3 — wrap the backend once.**

```go
type recordingBackend struct {
    inner mcp.Backend
    rec   *activity.Recorder
}
```

Every path now crosses exactly one recorder. The stdio bridge is not wrapped —
it is a different process with no store — so its calls are recorded when they
land on the engine's REST API, once.

### Carrying attribution across the stdio hop

The stdio bridge is a separate process, so the engine cannot see its
`clientInfo` unless it is told. On `initialize`, the bridge records what the
client said about itself and sends it on every subsequent HTTP call:

```
X-Drover-Client:  claude-code/2.1.4
X-Drover-Session: 7f3a9c1e
X-Drover-Via:     stdio
```

Headers, not body fields, so no request type changes. A request without them
is `source: cli`. A request the in-process `/mcp` handler makes sets the same
struct on the context directly — same shape, no serialisation.

Nothing in these headers is trusted for authorisation, because there is no
authorisation. They are labels on a log line, and the log is local.

---

## 3. The record

```go
type Record struct {
    ID       string    // sortable: <unix-nanos>-<4 random chars>
    At       time.Time
    Duration time.Duration

    Tool   string         // "grep", "git", "api_call", "sql_query"
    Op     string         // git/lsp sub-operation: "blame", "references"
    Args   map[string]any // as received, after redaction
    Reason string         // self-reported, may be empty

    Source    string // "mcp-http" | "mcp-stdio" | "cli" | "web"
    Client    string // "claude-code/2.1.4", "" when unknown
    Session   string
    SeqInSess int
    SincePrev time.Duration

    // What it touched, so an object page can filter to its own history.
    Repository string
    Object     string // request or connection name

    Outcome  string // "ok" | "error" | "empty" | "cancelled"
    Error    string
    Summary  string // "17 matches in 6 files (4,102 searched)"
    Bytes    int
    Truncated bool
}
```

`Summary` is the field the whole view lives on. It is built per tool, in the
recorder, from the typed response — not from a generic size:

| tool | summary |
|---|---|
| `grep` | `17 matches in 6 files · 4,102 searched · truncated` |
| `read` | `api/auth/session.go:1-240 · 8.1 KB` |
| `find` | `12 paths` |
| `ls` | `34 entries` |
| `git` | `blame api/auth/session.go:88 · 1 commit` |
| `lsp` | `references · 9 across 4 files · gopls` |
| `api_call` | `GET get-user → 200 · 1.2 KB · 143ms` |
| `sql_query` | `analytics · 200 rows (truncated) · 89ms` |

`Outcome: "empty"` is deliberately not `ok`. A grep that matched nothing is
the single most useful line in the log — it is what you look for when someone
says drover was no help — and it must be findable without reading summaries.

---

## 4. Redaction

drover's existing rule is that a secret is "never printed, logged, or shown to
an agent". An activity log is a log. The rule applies without exception.

- **Arguments are stored as received**, never as resolved. `api_call`'s record
  holds `{userId: "usr_1a2b"}` and the *declared* url
  `{{baseUrl}}/v1/users/{userId}`, never the resolved one — the resolved url
  has the environment interpolated into it, and an environment can carry a
  token in a query string.
- **Headers are never stored.** They resolve `${TOKEN}` by design.
- **No response bodies from `api_call`, no rows from `sql_query`.** Status,
  size, row count, elapsed. That is enough to see what happened and not enough
  to leak a customer record into a file that outlives the session.
- **File tools may keep a preview** — the first matched line, the read range.
  That content is already jailed, already agent-readable, and already came
  from a checkout.
- A final pass replaces any exact substring equal to a resolved secret value
  with `••••`, as a backstop for the case the rules above missed. Cheap, and
  it fails safe.

`activity.captureArgs: false` drops arguments entirely for anyone who wants
only the shape of what happened.

---

## 5. Storage

Two tiers, matching how the data is actually read.

**In memory: a ring of the last 1,000.** Every live view reads this. Fixed
size, no allocation per call beyond the record, no disk in the hot path. A
tool call must not wait on a write.

**On disk: `~/.drover/activity/YYYY-MM-DD.jsonl`.** Append-only, one JSON
object per line, written by a single goroutine draining a buffered channel. If
the channel is full the record is dropped and a counter increments — a slow
disk degrades the log, never the tool.

JSONL because it matches drover's own argument. This is a tool whose thesis is
that real files and real `grep` beat an index; putting its own history in a
database it would then have to query would be an odd thing to do on the way to
making that point. `grep '"outcome":"empty"' ~/.drover/activity/*.jsonl` is
the feature.

Retention: `activity.retainDays` (default 14), enforced by deleting whole day
files at startup and at midnight. No compaction, no rotation mid-file.

---

## 6. Wire API

Under the existing `/apis/drover/v1` prefix:

```
GET  /apis/drover/v1/activity            list, newest first
       ?limit= &before= &tool= &source= &session= &repository= &outcome= &q=
GET  /apis/drover/v1/activity/{id}       one record in full
GET  /apis/drover/v1/activity/sessions   sessions with counts and spans
GET  /apis/drover/v1/activity/stats      counts by tool/outcome/repo over a window
GET  /apis/drover/v1/events              SSE: live records + object changes
```

The list endpoint reads the ring when the window is inside it and falls back
to scanning day files when it is not. `before` is a record id, so paging is a
cursor and not an offset — the list grows at the head while you read it.

---

## 7. One push channel, two consumers

`/events` is a Server-Sent Events stream: `activity` frames as calls land,
`objects` frames when the store changes, `repo` frames when a sync finishes.

The same hub closes the gap the README currently apologises for.
`internal/mcp/http.go:38` answers GET with 405 and the docs say the HTTP
transport "has no channel for a server-initiated message" — but GET plus SSE
*is* that channel in Streamable HTTP, and `internal/mcp/notify.go` already has
the notification logic for stdio. Wiring the hub to a GET handler on `/mcp`
means an HTTP-connected agent gets `tools/listChanged` exactly like a stdio
one, and `initialize` can finally promise it on both transports.

One hub, two subscribers: the browser gets records, the agent gets tool-list
changes.

---

## 8. The web dashboard

### Routes

```
/dashboard                            overview — what the TUI summary shows
/dashboard/activity                   the full activity view
/dashboard/activity/<id>              one call, everything about it
/dashboard/sessions/<session>          one session as a chain
/dashboard/repositories               all repositories
/dashboard/repositories/<name>        one repository
/dashboard/requests/<name>            one HTTPRequest
/dashboard/databases/<name>           one SQLConnection
/dashboard/environments/<name>        one Environment
```

Every path serves the same HTML shell; routing happens client-side against the
JSON API, so a deep link is a real URL that survives a refresh and can be
pasted to someone.

`drover dash --web` opens `/dashboard` in a browser instead of drawing the
TUI. `drover serve` prints both URLs at startup.

### The overview

The TUI summary, in HTML: engine, uptime, data dir, then repositories,
requests, connections, environments — the same columns, the same `●`/`○` for
offered-versus-stored. Plus one thing the TUI has no room for: a live strip of
the last ten tool calls, which is what makes the page worth leaving open.

### The activity view

A dense table, newest first, streaming in over SSE:

```
TIME      TOOL       TARGET                SUMMARY                              SOURCE          MS
14:22:07  grep       api                   17 matches in 6 files                claude-code    124
          "finding where sessions are minted"
14:22:04  read       api/auth/session.go   lines 1-240 · 8.1 KB                 claude-code      3
14:21:58  git blame  api/auth/session.go   line 88 · 1 commit                    claude-code     41
14:21:44  sql_query  analytics             0 rows                                claude-code     67
```

Filters across the top — tool, source, outcome, repository, session, free text
over arguments and summary. Two of them earn their place immediately: **empty
results** answers "why did the agent not find it", and **slowest first**
answers "what is making this feel bad".

A row expands into the full record: every argument, the reason, the timing
split, the error, the preview if there is one. `/dashboard/activity/<id>` is
that row on its own page.

### A session as a chain

`/dashboard/sessions/<id>` is the view this feature exists for. One agent
connection, its calls in order, indented by gap — a burst of four calls in two
seconds is one thought, and a ninety-second pause is the model going away to
write something. Read top to bottom it is the actual reasoning trace, which
you cannot get from the agent's own transcript because the transcript does not
say what came back.

### An object page

`/dashboard/repositories/<name>` shows what `drover get` shows — url, branch,
interval, phase, commit, last sync, the error in full when it failed — plus
the applied yaml, plus **its own activity**: every tool call that touched this
checkout. That is the answer to "is this repository earning its disk", and it
is the same filter as the main view with `repository=` pinned.

The request page adds the parameter table and a **try it** button that calls
the same endpoint `drover call` does, GET-only, no `--allow-write` from a
browser. The connection page adds the health gate's last result and a query
box that runs through the same read-only path. Both of those record activity
with `source: web`, so the log tells the truth about who ran what.

### Look

Match the TUI, not a web framework. Monospace, dense, dark by default, the
same column names and the same `●`/`○` vocabulary, so the two dashboards read
as one product. No emoji, no icon glyphs, no purple.

### Dependencies — what this section said, and what happened

This originally said: one `index.html`, one `drover.css`, one `drover.js`, no
npm, no bundler, no framework, "vanilla DOM against `fetch` and `EventSource`
is enough for tables and filters". That held for about one release.

It is now React 19 + [nuqs][], built by Vite into `internal/web/dist` and
embedded with `go:embed`. Two things forced it:

1. **Every repaint threw the page away.** The vanilla version re-rendered the
   entire document on a two-second timer, which lost scroll position, closed
   anything expanded, and made each poll visible as a flicker. Keeping a live
   panel *and* a stable one means diffing, and hand-rolling a diff is writing
   a framework badly.
2. **View state belongs in the URL, and there was no discipline enforcing it.**
   The filters were read and written by hand in two functions that had already
   drifted. nuqs makes the URL the single source of truth for filters, sort,
   the open row, the active tab and the live toggle, so a link to what you are
   looking at is always a real link. `src/state.js` declares every parameter
   once — including which ones push a history entry, so the back button undoes
   the last thing you did.

The cost is honest: the bundle is ~246 KB raw, ~76 KB gzipped, against the
"well under 100 KB" this section promised. It is still one entry chunk, still
`go:embed`ed, still no build step in front of `go build` for anyone who is not
changing the dashboard, and still no runtime network dependency — the CSP is
`default-src 'self'` and the build emits no inline script, so nothing is
fetched from anywhere.

The rest of this section still holds. It is a read-mostly panel; nothing here
is a single-page application in the bad sense.

[nuqs]: https://nuqs.dev

---

## 9. Security — this is the part that changes

Adding a browser surface promotes an existing hole from theoretical to urgent.

**`/apis/drover/v1/*` has no Origin check and no Content-Type check.**
`decodeBody` (`server.go:722`) decodes JSON off any request regardless of what
it claims to be. `internal/mcp/http.go` guards `/mcp` against exactly this
attack and the REST API never got the same treatment. So a page you visit can
issue a simple cross-origin `POST` with `enctype="text/plain"` carrying a JSON
body to `/apis/drover/v1/apply` and register a Repository in your engine — no
preflight, no consent. The response is unreadable cross-origin, so it is blind
CSRF, but it is a write.

Three changes, all small, all prerequisites for shipping a browser UI:

1. **Lift `originAllowed` out of `internal/mcp/http.go` into shared
   middleware** and apply it to every route, not just `/mcp`.
2. **Require `Content-Type: application/json` on every write endpoint.** That
   alone forces a preflight for cross-origin requests, which kills the
   simple-request path.
3. **Serve the dashboard with `Content-Security-Policy: default-src 'self'`**
   and no inline script, so a stored value rendered into the page — a repo
   name, an error string from git, an argument in the activity log — cannot
   execute. Every value goes in as `textContent`, never `innerHTML`.

The activity log is also a new object worth thinking about: it sits in
`~/.drover` at `0600`, and it is the first file drover writes that contains
anything derived from a database or an API response. The redaction rules in §4
are what keep it boring.

---

## 10. The TUI gets it too

`drover serve` on a tty already toggles between summary and tables with **d**.
Add a third: **a**, a live activity pane — the same last-N ring, the same
columns, scrolling as calls arrive. It is the view you actually want open on a
second monitor while an agent works, and the data is already in memory.

---

## 11. Config

```yaml
# ~/.drover/config.yaml
activity:
  enabled: true          # record at all
  askWhy: true           # offer the optional `reason` argument on each tool
  captureArgs: true      # store arguments (redacted); false stores only shape
  captureBodies: false   # never on for api_call/sql_query; file previews only
  retainDays: 14
  ringSize: 1000
dashboard:
  enabled: true          # serve /dashboard
```

`enabled: false` compiles the same but registers no recorder and no routes, so
someone who wants a silent engine gets one.

---

## 12. Build order

Each phase is useful on its own and leaves the tree green.

1. ~~**Context through MCP.**~~ **Built.** `handler` takes a `ctx`; both
   transports thread it; the `Backend` interface takes one on every method,
   and the four `context.Background()` calls that were throwing away
   cancellation the git, lsp, http and sql layers already supported are gone.
   `internal/files` takes one too, checked per entry, because a grep across
   every checkout is the longest thing drover does on its own CPU. The CLI
   installs one signal-aware context in `main` and passes it to every command.
   Proved by `TestHTTPToolCallIsCancelledWhenTheClientHangsUp` and the two
   cancellation tests in `internal/files`.
2. ~~**Collapse the REST handlers onto `backend`.**~~ **Built.** `handleLs`,
   `handleRead`, `handleGrep` and `handleFind` are now decode-delegate-write
   adapters over the backend instead of a second implementation of it.
3. **`internal/activity`.** Record, Recorder, ring, JSONL writer, redaction,
   per-tool summaries. Unit-tested without a server.
4. **Wrap the backend; add the attribution headers to the stdio bridge.**
   Activity is now real and inspectable with `cat`.
5. **The `reason` argument** on every tool schema.
6. **The activity HTTP API**, plus the SSE hub.
7. ~~**`tools/listChanged` over HTTP.**~~ **Built**, on its own hub in
   `internal/mcp/stream.go`: a GET carrying `Accept: text/event-stream` opens
   the stream, the tool watcher runs only while somebody is listening, and
   `initialize` now promises `listChanged` on both transports. A GET without
   that Accept keeps the old 405. Live-verified against a running engine.
8. ~~**Origin, Content-Type.**~~ **Built.** One `guard` in front of every
   route, sharing its rules with the MCP transport via `internal/httpguard`.
   CSP still to come, with the HTML.
9. **The dashboard**: shell and overview, then activity, then object pages,
   then sessions.
10. **The TUI activity pane.**

Phases 1, 2, 7 and 8 are worth doing even if the dashboard never gets built.

---

## 13. Decisions worth revisiting

- **A self-reported `reason` argument.** It is the only thing that answers the
  literal question, and it is a claim by an interested party. Rendered in
  quotes, never aggregated into a statistic, never used to make a decision.
  If it turns out models write filler, it costs tokens for nothing and should
  be defaulted off.
- **JSONL, not SQLite.** A day of heavy use is maybe a few thousand lines, and
  a filter over that is a linear scan of a small file. If someone runs an
  engine that logs a million calls a week this is the wrong choice — but a
  tool that argues real files beat an index should hold its own history in
  real files until measurement says otherwise.
- **No response bodies for `api_call` and `sql_query`.** Losing the ability to
  see what an agent actually got back is a real cost when debugging. It is
  still right: the log outlives the session, and a customer row in a file in
  `~/.drover` is a leak with no upside.
- **Attribution headers are unauthenticated.** Anything can claim to be
  `claude-code`. This is a local log with no authorisation attached to it, so
  the labels are descriptive and nothing hangs on them. If drover ever grows a
  bearer token (§8 of the review), attribution should move behind it.
- **Client-side routing rather than server-rendered pages.** One shell and one
  API is less code and gives the live view for free. It means no dashboard
  without JavaScript, which for a localhost cockpit is an acceptable trade.
- **`Outcome: "empty"` as its own outcome, not a successful call with zero
  results.** It is a judgement — an empty grep is a *fine* outcome sometimes —
  but making it filterable is worth more than the purity.

---

## 14. Non-goals

- **No authentication.** Unchanged. `/dashboard` is as reachable as `/mcp` is,
  which is to say from that machine only. Binding to a network interface is
  still the user's decision, and now it exposes a UI as well as an API.
- **No writes to the object store from the browser.** The dashboard reads,
  triggers a sync, re-runs a health check, and calls a GET request. It does
  not apply, edit or delete objects. Objects come from yaml; that is the
  model, and a web form that quietly becomes a second source of truth would
  break it.
- **No metrics export, no Prometheus endpoint, no charts over time.** The
  question this answers is "what did the agent just do and why did it not
  help", not "what is the p99".
- **No cross-engine view.** `--server` points a dashboard at one engine.
  Aggregating several is a different product.
