# drover — plan

A Go engine that holds git checkouts, HTTP requests and (later) a database in
one place and hands coding agents real filesystem tools over MCP. Not
embeddings.

This directory is the Go module. Binary name is `drover`.

## What it is

kubectl / apiserver, but for context.

- **Server** (`drover serve`) is the engine. It owns clones, the sync loop,
  applied objects, and later MCP / HTTP requests / SQL.
- **Client** (`drover apply`, `get`, `delete`) talks to that server. YAML can
  live anywhere. The engine can run anywhere.
- **No token** on apply. Default listen is `127.0.0.1` so only that machine
  can apply unless you bind on the network on purpose.

Thesis: an agent with `grep` / `ls` / `find` / `read` against a real checkout
beats a vector index.

## Non-goals (this plan)

- Embeddings, treesitter indexes, knowledge graphs.
- Watching config files on disk (change → `apply -f` or restart serve).
- Kustomize, Helm, CRDs, namespaces, `kubectl apply --server-side`.
- Authn/authz on the apply API.
- Writing anything. Every tool an agent gets is read-only: GET requests only,
  read-only SQL, and no file tool that writes.

## Naming rule

**Kinds are spelled out. No shortforms.** `Repository`, not `Repo`.
`HTTPRequest`, not `Curl`. `SQLConnection`, not `SQL`. The CLI matches the
kind (`drover get repository`) — there are no `kubectl`-style short aliases,
on purpose.

## Object model

Every document:

```yaml
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
```

Rules:

- One document = one object.
- Identity is `(kind, metadata.name)`.
- Unknown `kind` → error.
- `---` multi-doc files are valid.
- Apply is declarative: first time creates, second time updates.
- Apply is **atomic per invocation**: any document invalid → nothing is
  written, nothing is cloned.

Four kinds:

| kind             | one document is           | now                    |
|------------------|---------------------------|------------------------|
| `Repository`     | one git repo              | implement              |
| `Environment`    | one named stage of vars   | parse/reject only      |
| `HTTPRequest`    | one HTTP request          | parse/reject only      |
| `SQLConnection`  | one database              | parse/reject only      |

### Names and uniqueness

`metadata.name` is the primary key and it also becomes a directory name, so
it is validated hard:

- RFC 1123 label: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, 1–63 chars.
- Lowercase only. This is not cosmetic — macOS filesystems are
  case-insensitive, so allowing `API` and `api` would produce two objects that
  fight over one clone directory.
- `.`, `..`, `/`, and anything that could escape the data dir are rejected by
  construction.

**Two objects of the same kind can never share a name.**

- Duplicate `(kind, name)` inside one apply batch (same file, or two files in
  the same `-f` directory) → the whole apply is rejected, listing both source
  files. It is never "last one wins".
- Re-applying a name that already exists in the store is an **update** of that
  one object, not a second object. That is the declarative path and it is
  fine.
- The update records the new source path, so an object that moved from one
  yaml file to another stops being attributed to the old file.
- Names are free to repeat *across* kinds: `Repository/api` and
  `HTTPRequest/api` are different objects.

### Repository spec

```yaml
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
  refreshInterval: 15m        # optional, per repository
```

`url` and `branch` required. Clone **that branch only** (`--single-branch`).
Checkout path on the server: `$DROVER_DATA/repos/<metadata.name>/`.

`refreshInterval` is **per object** — a hot monorepo can pull every 5m while a
vendored reference repo sits at 24h. Resolution order:

1. `spec.refreshInterval` on the object
2. `--sync` on `drover serve`
3. built-in default `1h`

Accepts Go durations (`30s`, `15m`, `24h`). `0` or `never` means this repo is
only refreshed when it is re-applied or explicitly synced — the ticker skips
it. Minimum accepted non-zero value is `30s`; below that it is rejected at
parse time rather than melting a git host.

Private GitHub uses the environment the server already has (`GITHUB_TOKEN`,
`gh auth`, ssh). Nothing secret in yaml.

### Environment spec (later)

A named stage. This is what makes `HTTPRequest` reusable instead of
copy-pasted three times.

```yaml
apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
spec:
  description: production, read-only creds
  variables:
    baseUrl: https://api.acme.com
    tenant: acme
  secrets:
    token: ${ACME_PROD_TOKEN}
```

```yaml
apiVersion: drover/v1
kind: Environment
metadata:
  name: local
spec:
  variables:
    baseUrl: http://127.0.0.1:3000
    tenant: dev
  secrets:
    token: ${LOCAL_TOKEN}
```

- `variables` — plain values. Safe to print, safe to show an agent.
- `secrets` — same shape, but the value must be a `${ENV}` reference and is
  **never** echoed back by `get`, never logged, never shown in an MCP tool
  description. `get environment prod` prints `token: <set from
  ACME_PROD_TOKEN>` or `<unset>`.
- A secret whose env var is missing on the server does not fail apply. It
  fails at execute time, with a message naming the variable.

### HTTPRequest spec (later)

One request per document. Everything a curl can carry — method, url, headers,
query, path params, body — with names and descriptions on all of them, because
those descriptions are what the agent reads to fill the call in.

```yaml
apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  description: Fetch one user by id. Use when you have a user id already.
  method: GET
  url: "{{baseUrl}}/v1/users/{userId}"

  environments: [local, stage, prod]
  defaultEnvironment: stage

  pathParams:
    - name: userId
      description: Opaque user id, looks like usr_1a2b3c
      required: true
      example: usr_1a2b3c

  query:
    - name: include
      description: Comma-separated relations to expand
      required: false
      example: profile,orgs
    - name: tenant
      description: Tenant slug
      required: false
      default: "{{tenant}}"

  headers:
    - name: Authorization
      value: "Bearer {{token}}"
    - name: Accept
      value: application/json

  body:
    contentType: application/json
    template: |
      {"note": "${OPERATOR}", "tenant": "{{tenant}}"}
```

- `environments` lists the stages this request is valid in. Omitted → every
  `Environment` object applies. A stage named here that does not exist as an
  `Environment` object → apply error, because that is a typo, not a feature.
- `defaultEnvironment` is what MCP uses when the agent does not pick one; it
  must be in `environments`. Omitted with more than one stage → the agent must
  pass `environment` explicitly.
- MCP will only **advertise and execute GET**. POST/PUT/DELETE may be stored;
  they are never offered to the client.

### Placeholders

Three syntaxes, because there are genuinely three different sources, and
collapsing them would mean an agent could inject an env var or a secret.

| syntax     | resolved from                          | resolved when | agent can set |
|------------|----------------------------------------|---------------|---------------|
| `{{name}}` | selected `Environment` vars + secrets  | execute time  | no            |
| `${NAME}`  | the server process environment         | execute time  | no            |
| `{name}`   | a declared `pathParams` / `query` entry| per call      | **yes**       |

Rules:

- `{{name}}` and `${NAME}` are server-side. They never appear as MCP tool
  parameters and their values are never returned in a tool description.
- `{name}` must have a matching declared param, and every declared path param
  must appear in the url. Mismatch either way → apply error.
- Resolution is single-pass. A value that itself contains `{{...}}` is not
  re-expanded — no recursion, no expansion bombs.
- An unresolved `{{name}}` at execute time is an error naming the variable and
  the stage, not an empty string silently pasted into a url.
- Substitution into url path and query is percent-encoded. Into headers, it is
  rejected if the value contains CR or LF.

### SQLConnection spec (later)

```yaml
apiVersion: drover/v1
kind: SQLConnection
metadata:
  name: prod
spec:
  url: ${DATABASE_URL}
  health: SELECT 1
```

If `health` is present **and** it succeeds, advertise a `sql` tool. No health
or failed health → no SQL tool.

## `~/.drover` — the data dir

Everything the engine knows lives here, so a restart forgets nothing. Default
`~/.drover`, override with `--data-dir` / `DROVER_DATA`.

```
~/.drover/
  config.yaml                       # servers, listen, apply provenance
  objects/
    Repository/api.yaml             # the applied document, verbatim
    Environment/prod.yaml
    HTTPRequest/get-user.yaml
    SQLConnection/prod.yaml
  repos/
    api/                            # the clone
```

**Apply persists.** On a successful apply the server writes each document to
`objects/<Kind>/<name>.yaml` before it reports success. On boot, `serve` loads
`objects/` first — that is the desired state, not the client's yaml files. The
client's files are just how the state got here; they can be deleted or the
laptop reformatted and the engine still knows what it holds.

Written documents carry a small provenance header the client did not send:

```yaml
# drover: applied 2026-08-20T01:22:04Z from /Users/shekhar/work/repo-api.yaml
apiVersion: drover/v1
kind: Repository
...
```

Writes are atomic — temp file in the same directory, `fsync`, rename — so a
kill mid-apply cannot leave a half-written object that poisons the next boot.
A file under `objects/` that fails to parse on boot is a hard startup error
naming the file, never a silent skip.

`delete` removes `objects/<Kind>/<name>.yaml` and, for `Repository`, the clone.

## `~/.drover/config.yaml`

One file. Client uses it to find the engine. Serve uses it to listen and to
bootstrap apply.

```yaml
current: home
servers:
  home:
    url: http://127.0.0.1:7432

listen: 127.0.0.1:7432
sync: 1h
apply:
  - /Users/shekhar/hub/configs
  - /Users/shekhar/work/repo-api.yaml
```

- `current` + `servers` — client. `--server` / `DROVER_URL` override.
- `listen` — serve bind. Default `127.0.0.1:7432` (walk the port if busy,
  like digg).
- `sync` — default refresh interval for repositories that do not set their
  own. `--sync` on the command line wins.
- `apply` — paths applied **before** the server accepts traffic. A path is a
  file, or a directory of `*.yaml` / `*.yml` (one level, like `kubectl apply
  -f dir`).
- No `token` field.

**Apply updates this file.** A successful `drover apply -f <path>` appends
that path (absolute, symlinks resolved, deduped) to `apply:` and rewrites
`config.yaml` atomically. So the next `drover serve` re-reads the same sources
you have been applying, without you maintaining the list by hand.

- `-f -` (stdin) has no path to record. The objects still persist under
  `objects/`; nothing is added to `apply:`.
- `--no-remember` on apply skips the config update.
- A path that no longer exists on a later boot is a startup error, same as
  today — with a hint to run `drover forget <path>`.
- `delete` prunes a path from `apply:` once no stored object is attributed to
  it any more, which is what the provenance header is for.
- The rewrite is a marshal, not a round-trip: comments and key order in
  `config.yaml` are **not** preserved. Documented rather than fixed.

Client lookup for this file: `--config`, else `DROVER_CONFIG`, else
`$DROVER_DATA/config.yaml`, else `~/.drover/config.yaml`.

## CLI

```
drover serve                       # engine
drover apply -f <file|dir>         # also -f - for stdin, --no-remember
drover get repository
drover get repository api
drover delete repository api
drover sync                        # force a refresh now, all repositories
drover sync api                    # force one
drover forget <path>               # drop a path from config apply:
drover version
drover help
```

`drover` with no args prints help (unlike digg, this is not a browser app).
Serve is explicit. Kind arguments are the full kind, lowercased —
`repository`, `environment`, `httprequest`, `sqlconnection`. Plurals are
accepted (`drover get repositories`) since it reads better; abbreviations are
not.

Apply flags: `-f` repeatable. Directory = all yaml in that directory, not
recursive.

## Server

HTTP, JSON, no token.

```
PUT    /apis/drover/v1/repositories/:name     apply/upsert
GET    /apis/drover/v1/repositories           list
GET    /apis/drover/v1/repositories/:name     get
DELETE /apis/drover/v1/repositories/:name     delete (object + clone)
POST   /apis/drover/v1/repositories/:name/sync
```

Same pattern for `environments`, `httprequests`, `sqlconnections`. Applying
an unsupported kind returns 400 with a clear body, nothing stored.

Apply of a multi-document batch is one request per object, but the **client**
validates the whole batch (schema, duplicate names, cross-references) before
it sends the first one, so a bad document in file three does not leave files
one and two applied.

## Repository reconcile

On apply (and on that repository's own tick):

1. If `$DROVER_DATA/repos/<name>` is missing →
   `git clone --single-branch --branch <spec.branch> <url> <path>`
2. If it exists → `git fetch origin <branch>` and
   `git checkout -B <branch> origin/<branch>`
   (reset to the remote branch; this tree is a mirror, not a worktree people
   commit in)
3. `spec.url` or `spec.branch` change → treat as a new desired remote/branch
   and move HEAD to match. Do not require delete+reapply.

Step 2 discards anything local, which is intended — but only for trees drover
created. If `repos/<name>` exists and is not a clone of `spec.url`, reconcile
refuses and reports, rather than resetting a directory that might be someone's
real work.

The sync loop is one ticker at the shortest configured interval; each tick
reconciles only the repositories that are due. Failures are recorded on the
object (last error, last success) and retried on the next tick — a dead remote
does not stop the others.

`drover get repository` shows that status, so a clone that has been failing
auth for three days is visible instead of silent.

Delete: drop the object file and `os.RemoveAll` the clone.

## MCP

Built — see the MCP section at the end for what shipped. `drover mcp` is a
stdio JSON-RPC bridge to the engine. Agents never apply.

## Go layout

This module, digg-shaped, no web UI.

```
cmd/drover/main.go           # parse args, dispatch
internal/config/             # ~/.drover/config.yaml, apply-list updates
internal/object/             # parse yaml docs, kinds, validation (pure)
internal/client/             # apply/get/delete HTTP
internal/server/             # listen, routes, startup apply
internal/store/              # durable objects on disk
internal/repo/               # clone, fetch, delete
internal/sync/               # per-object refresh ticker
internal/httpreq/            # execute an HTTPRequest
internal/sqldb/              # postgres / mysql / redshift
internal/files/              # ls, read, grep, find, jailed to the checkouts
internal/mcp/                # JSON-RPC over stdio
```

`internal/object` has tests without git or HTTP. Git tests skip when
offline / no git.

## Build order

1. **Object + config** — DONE. `internal/object`, `internal/config`.
2. **Store + HTTP** — DONE. `internal/store`, `internal/server`,
   `internal/client`, `cmd/drover`.
3. **Clone + sync** — DONE. `internal/repo`, `internal/sync`.
4. **MCP** — DONE. `internal/files`, `internal/mcp`.
5. **Environment + HTTPRequest** — DONE. `internal/httpreq`.
6. **SQLConnection** — DONE. `internal/sqldb`, providers postgres / mysql /
   redshift.

All six slices are built. MCP wraps the same engines the CLI drives, rather
than reimplementing them.

## MCP

Two transports, one router, so neither can support a method the other does
not.

**HTTP, on the engine itself.** `drover serve` hosts MCP at `/mcp`, and prints
the URL at startup. Nothing extra to run.

```
claude mcp add --transport http drover http://127.0.0.1:7432/mcp
```

The handler is wired to an in-process backend, so a tool call does not travel
back through the engine's own listener.

**stdio, for clients that only speak it.** `drover mcp` bridges an agent's
stdin/stdout to an engine over HTTP, which may be on another machine.

```
claude mcp add drover -- drover mcp
```

Both verified connected from Claude Code.

`/mcp` accepts POST. GET is 405 — that would be a server-initiated stream and
drover never initiates anything. DELETE is 204, since there is no session to
end. A notification gets 202 with no body. Batches are answered even though
the newest revision dropped them, because older clients still send them.

**The `Origin` check is the whole defence.** A visited web page can POST to a
localhost port, so a request carrying an `Origin` must have a loopback one or
it gets 403 — the DNS-rebinding attack the transport spec calls out for local
servers. A request with no `Origin` is a direct client and is allowed. drover
has no auth by design, which is exactly why this check has to be there.

Tools, four fixed plus one per applied object:

| tool | does |
|------|------|
| `ls` | list a directory; no path lists the repositories |
| `read` | read a file, numbered lines, offset/limit windows |
| `grep` | RE2 search, `path` and `include` filters |
| `find` | glob on the file name, or on the whole path when it has a slash |
| `call_<name>` | one configured HTTP GET |
| `query_<name>` | one read-only query against a healthy database |

What is never advertised: a non-GET `HTTPRequest`, and a `SQLConnection` whose
health query has not passed. Both are stored, neither is offered.

The file tools are jailed to `$DROVER_DATA/repos`. The jail rejects `..`
lexically *and* resolves symlinks and re-checks the target, because a link
inside a checked-out repository pointing at `/etc/passwd` contains no dots at
all, and repository contents are written by whoever wrote the repository.
Walks never follow a symlink out. `.git` is skipped; `.gitignore` and
`.github` are not.

A tool that runs and fails returns `isError` with the reason in the text,
which the model can read and react to. A JSON-RPC error is reserved for a call
that could not be made at all — an unknown tool, or malformed params.

### What is built

Verified live: repositories cloned from GitHub; `drover call` against the real
GitHub API resolving all three placeholder sources; `drover query` against
Postgres 16 and MySQL 8 in Docker.

- **Repository** — clone, per-object `refreshInterval`, one timer each,
  ownership marker, status.
- **Environment** — `variables` plus `secrets`; a secret must be a single
  `${ENV}` reference, is redacted everywhere, and `get` shows only which
  variable backs it and whether that variable is set.
- **HTTPRequest** — three placeholder syntaxes, `{name}` ↔ `pathParams`
  agreement enforced at apply, GET-only execution unless `--allow-write`,
  parameters percent-encoded into path and query, unknown parameters refused,
  1 MiB response cap, only `Content-Type`/`Content-Length`/`Location` echoed
  back.
- **SQLConnection** — postgres, mysql, redshift. Redshift rides the pgx driver
  but keeps its own name and dialect note. Health gates the tool. `readOnly`
  defaults to **true** and is enforced in the query path, not just at the tool
  boundary. `maxRows` defaults to 200 and reports truncation.

### CLI

```
drover serve
drover apply -f <file|dir>
drover get <kind> [name] [-o table|yaml|json]
drover delete <kind> <name>
drover sync [name]
drover call <name> [-p k=v] [--environment prod] [-o body|json|head]
drover query <name> "SELECT ..." [-o table|json|csv]
drover health <name>
drover forget <path>
drover mcp
```

### Decisions worth revisiting

- **`readOnly: true` by default** on SQLConnection. The plan did not ask for
  this; a database tool handed to an agent should not be able to write, and
  opting out should be a deliberate line in a file. The gate is a keyword
  check that fails closed, refuses stacked statements, and looks past leading
  comments — not a SQL parser.
- **Headers may not interpolate `{param}`.** A caller-settable header is a
  credential-theft and request-smuggling primitive, so headers resolve only
  `{{environment}}` and `${processEnv}` values.
- **A literal in `secrets:` is refused**, including `"Bearer ${TOKEN}"` —
  only a bare `${ENV}` reference. Concatenation belongs in the request.
- **`Curl` is named `HTTPRequest`**, for the same reason `Repo` is
  `Repository`.
- Stored objects are a YAML re-marshal with a provenance header, not a
  byte-for-byte copy of the applied file.
- `spec.url` on a Repository also accepts an absolute local path.
