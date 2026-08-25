# drover

kubectl, but for context.

drover holds git checkouts, HTTP requests and databases in one place, and hands
coding agents real filesystem tools over MCP.

An agent with `grep` / `ls` / `find` / `read` against a real checkout beats a
vector index. drover is that checkout, kept fresh, plus the API calls and
queries you already had lying around in scratch files.

**Not embeddings.** No index, no chunking, no similarity search. Real files on
disk and real `grep`, so the answers are exact.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/notshekhar/drover/main/install.sh | bash
```

Windows:

```powershell
irm https://raw.githubusercontent.com/notshekhar/drover/main/install.ps1 | iex
```

One static binary, ~4.5 MB. No runtime, no cgo, so the same Linux asset runs on
Alpine and Debian alike. macOS, Linux, Windows and FreeBSD; x64 and arm64.

Upgrade later with:

```bash
drover upgrade
```

<details>
<summary>Other ways</summary>

```bash
# see whether a newer release exists, without installing it
drover upgrade --check

# a specific release
drover upgrade --version v0.4.0
curl -fsSL .../install.sh | bash -s -- --version v0.1.0

# from source
go install github.com/notshekhar/drover/cmd/drover@latest

# remove it (your data in ~/.drover is left alone)
curl -fsSL .../install.sh | bash -s -- --uninstall
```
</details>

## Use it

Start the engine:

```bash
drover serve
```

That draws a dashboard — what it holds, when each repository last synced, which
requests and databases an agent is actually offered:

```
 drover 0.2.0
────────────────────────────────────────────────────────────────────────────
 engine  http://127.0.0.1:7432   MCP  http://127.0.0.1:7432/mcp
 data    /Users/you/.drover   uptime  2h0m

 REPOSITORIES (3) ──────────────────────────────────────────────────────────
   NAME             BRANCH    STATUS     COMMIT    REFRESH   LAST SYNC
   api              main      ready      b045e541  15m       3m ago
   vendored-docs    main      ready      99aa88bb  never     1d ago
   private-thing    main      failed     -         1h        never
      clone https://github.com/acme/private: remote: Repository not found.

 HTTP REQUESTS (2) ─────────────────────────────────────────────────────────
   NAME               METHOD  ENVIRONMENT  URL
 ● get-user           GET     prod         {{baseUrl}}/v1/users/{userId}
 ○ create-issue       POST    prod         {{baseUrl}}/issues

 SQL CONNECTIONS (1) ───────────────────────────────────────────────────────
   NAME               PROVIDER   ACCESS          STATUS
 ● analytics          postgres   read-only       ready
────────────────────────────────────────────────────────────────────────────
 r reload configs   s sync repos   q quit
```

`●` means an agent is offered it; `○` means it is stored but never advertised
— a non-GET request, or a database whose health check has not passed.

**Edits apply themselves.** drover watches its config and the yaml in
`~/.drover`, so saving a file is enough — there is nothing to press. **d**
switches between this summary and the full tables, **s** refreshes every
repository now.

`drover dash` opens the same screen for an engine that is already running,
including one on another machine (`--server`). `drover serve --no-tui` logs
plainly instead, which is what you want under systemd or in CI.

## Adding things

The first `drover serve` writes **`~/.drover/docs.md`** — a full reference for
every kind, written to be handed to an agent:

> read ~/.drover/docs.md and add our three repos and the billing API

Any `*.yaml` you or your agent drops in `~/.drover/` is applied on startup and
on reload. Nothing to register, no list to maintain.

```
~/.drover/
  docs.md          the reference
  config.yaml      drover's own settings — not an object file
  repos.yaml       anything else here gets applied
  github-api.yaml
  databases.yaml
```

Or keep the files anywhere and point at them with `drover apply -f`.

Nobody hand-writes forty request files, so if you already have an OpenAPI spec
or a Bruno collection:

```bash
drover import openapi -f openapi.yaml --prefix github --tag billing
drover import bruno   -f ./my-collection
```

It prints yaml for you to read, edit and commit — an importer's guesses are the
part worth looking at. `--out <file>` writes it, `--apply` applies it. A spec
with more than 40 operations refuses and tells you the count, rather than
producing 400 documents nobody asked for.

Tell it about a repository:

```yaml
# repo.yaml
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
  refreshInterval: 15m
```

```bash
drover apply -f repo.yaml
drover get repository
```

```
NAME   URL                           BRANCH   REFRESH   STATUS   COMMIT
api    https://github.com/acme/api   main     15m0s     ready    b045e541
```

Point an agent at it:

```bash
claude mcp add --transport http drover http://127.0.0.1:7432/mcp
```

The agent now has `ls`, `read`, `grep` and `find` over that checkout.

## What you can apply

Five kinds. Names are spelled out — `Repository`, never `Repo`.

### Repository

A git checkout, kept in sync on its own schedule.

```yaml
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
  refreshInterval: 15m      # or: never
```

`refreshInterval` is per repository, so a monorepo you work in daily can pull
every few minutes while a vendored reference sits at 24h. Each gets its own
timer, so one slow fetch never delays another.

The tree is a **mirror**: every sync resets it to the remote. drover refuses to
touch a directory it did not create, so it cannot eat a checkout of yours that
happens to share a name.

#### The discussion, not just the code

`git` says what changed and who changed it. It cannot say **why** — why was
argued in a pull request and filed in an issue, and neither is in the clone.

```yaml
spec:
  mirror:
    issues: true
    pullRequests: true
    since: 365d      # 90d, 6h, or all
```

Issues and pull requests land as markdown with a YAML header, reached as
`mirrors/api/...`, so `grep '^state: open' mirrors/api/issues` works and so
does grepping the argument itself.

The file that makes it worth having is **`mirrors/api/index/commits.tsv`**,
which maps a commit to the pull request that carried it. It is built from the
checkout's own history at no API cost — both of GitHub's merge styles put the
number in the commit subject — so an agent can go:

```
git blame  →  a1b2c3d  →  grep a1b2c3d mirrors/api/index/commits.tsv  →  pull 5678  →  read it
```

Four hops from a line of code to the argument about it. drover uses `gh` when
it is on PATH (so enterprise SSO already works) and `GITHUB_TOKEN` otherwise.
A large repository backfills across several syncs rather than stalling one,
and a rate limit is recorded with its reset time rather than treated as a
failure. A mirror that fails never marks the repository failed — the checkout
is still perfectly searchable.

#### Letting a repository describe itself

A service knows which API it calls. Commit a `.drover.yaml` at its root and
drover will read it — and then **quarantine it**, because a yaml file inside a
clone is written by whoever can push to that repository:

```bash
drover review api        # prints exactly what would be applied
```

Nothing is applied until the Repository says `spec.trustConfig: true`. There is
no command that flips a hidden switch: trust is a line in the document you
already control. `Repository` and `SQLConnection` are never accepted from a
repository at all — a clone target and a database url are the two things that
reach the network on drover's own credentials. Names are namespaced
(`api.get-user`) and labelled `drover.io/source: repository/api`, a prefix a
document cannot forge.

### Environment

A named stage — `local`, `stage`, `prod` — that requests fill their
placeholders from.

```yaml
apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
spec:
  variables:
    baseUrl: https://api.acme.com
    tenant: acme
  secrets:
    token: ${ACME_PROD_TOKEN}
```

A secret must be a bare `${ENV}` reference, never a literal, so a credential
cannot be committed by accident. Values are read from the engine's own
environment and are never printed, logged, or shown to an agent.

### HTTPRequest

One callable request, parameterised so an agent can actually use it.

```yaml
apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  description: Fetch one user by id.
  method: GET
  url: "{{baseUrl}}/v1/users/{userId}"
  environments: [local, stage, prod]
  defaultEnvironment: stage
  pathParams:
    - name: userId
      description: Opaque user id, looks like usr_1a2b3c
      required: true
      example: usr_1a2b3c
```

```bash
drover call get-user -p userId=usr_1a2b3c --environment prod
```

Three placeholder syntaxes, kept apart on purpose:

| syntax | resolved from | can the caller set it? |
|---|---|---|
| `{{name}}` | the selected Environment | no |
| `${NAME}` | the engine's process environment | no |
| `{name}` | a declared parameter | **yes** |

Only the third ever becomes a tool parameter, so an agent cannot reach a secret
by asking for it. Every `{name}` in the url must have a declared parameter and
vice versa — checked at apply, so an advertised tool is always honest about
what it needs.

**Only GET is offered to agents.** Other methods can be stored, and `drover
call --allow-write` will run one for you, but they are never advertised.

### DocumentStore

The one place an agent can write.

```yaml
apiVersion: drover/v1
kind: DocumentStore
metadata:
  name: product
spec:
  description: PRDs, TRDs and decision records for the billing platform.
  writable: true        # default
  history: true         # default
```

Everything else drover offers is read-only, which leaves a gap: the warehouse
has nowhere to put what an agent worked out, so every session starts from
nothing. Documents live at `documents/product/...`, are read with the `read`
and `grep` an agent already has, and are written with **`doc_write`** — the
only tool in drover that changes anything.

**Every store is a local git repository.** Each write is a commit whose author
is the agent's attribution and whose message is its stated reason, so "who
wrote this and why" is answerable with the `git` tool that already exists, and
nothing an agent writes is unrecoverable. It has no remote and drover never
pushes.

Markdown only, a write replaces the whole file, and an identical write is not
a commit. `documents/` and `repos/` are separate trees because they have
opposite durability rules: a checkout is disposable and a store is the truth.

### SQLConnection

Postgres, MySQL or Redshift.

```yaml
apiVersion: drover/v1
kind: SQLConnection
metadata:
  name: analytics
spec:
  provider: postgres        # postgres | mysql | redshift
  url: ${DATABASE_URL}
  health: SELECT 1
  maxRows: 200
```

```bash
drover query analytics "SELECT count(*) FROM events"
```

**Read-only by default.** Writes are refused, stacked statements are refused,
and a leading comment cannot disguise a write. Set `readOnly: false` to opt out
deliberately. `health` is a gate: no health query, or a failing one, and no sql
tool is offered at all.

**The schema lands on disk.** When the gate passes, drover dumps every table,
column, foreign key, index and row-count estimate to `docs/schema/<name>.sql`
as DDL — so discovering the shape costs a `read` instead of a round trip, and
`grep 'REFERENCES users' docs/schema` answers "what points at users" without
opening a connection. Row counts are labelled as the estimates they are.
`spec.schemas: [public, billing]` narrows it on a warehouse.

## MCP

`drover serve` hosts MCP at `/mcp`; it prints the URL at startup.

```bash
claude mcp add --transport http drover http://127.0.0.1:7432/mcp
```

For a client that only speaks stdio:

```bash
claude mcp add drover -- drover mcp
```

An agent is told what this engine holds, and what it is for, before it calls
anything. The connection handshake carries three things: when to reach for
drover at all — grounding a plan in how the thing is actually done today,
following a bug across a service boundary, settling a claim about code nobody
has open — then which tool answers which question, then the actual contents.
The repositories by name with their branch and how long ago each synced, the
environments, how many requests are callable, which databases are queryable.
There is no first call spent finding out, and no guessing at a repository name.

That list is a snapshot from when the agent connected, so two resources keep it
honest:

| resource | what |
|---|---|
| `drover://reference` | how to configure drover — every kind, its fields, the placeholder rules |
| `drover://inventory` | what the engine holds *now*, re-readable at any time |

Over stdio, drover also announces when its tool list changes. Apply a database
and `sql_query` appears; drop a yaml file in `~/.drover` and the request tools
arrive — a connected agent is told, without reconnecting. The HTTP transport
has no channel for a server-initiated message, so it does not advertise that it
will send one.

Tools an agent gets:

| tool | does |
|---|---|
| `ls` | list a directory; no path lists the repositories and the other roots |
| `read` | read a file, numbered lines, offset/limit windows |
| `grep` | RE2 search, with `path`, `include` and `selector` filters; skips `node_modules`, `vendor`, `dist` and the rest |
| `find` | glob on the name, or on the whole path when it has a slash; skips the same directories |
| `git` | history: `log`, `show`, `diff`, `blame`, `search`, `file`, `branches`, `tags`, `contributors`, `status` |
| `lsp` | code by meaning: `definition`, `references`, `hover`, `implementations`, `document_symbols`, `workspace_symbols`, `incoming_calls`, `outgoing_calls`, `diagnostics`, `servers` |
| `api_list` | find a request by fuzzy search over everything it says; also lists the environments |
| `api_describe` | one request's parameters in full |
| `api_call` | perform a request |
| `sql_query` | one read-only query against a named connection |
| `doc_write` | write one markdown document into a store — the only tool that changes anything |

**Eleven tools, fixed.** Twenty requests do not become twenty tools — they
become entries `api_list` returns, the databases are listed inside
`sql_query`'s own description, the stores are listed inside `doc_write`'s, and
ten history operations sit behind `git`'s `operation` argument rather than
becoming ten more tools.

`doc_write` is offered only when a writable `DocumentStore` exists, so an
engine with no store advertises no way to write at all.

drover also answers **`prompts/list`** with prompts generated from what this
engine actually holds — `investigate`, `onboard`, and `schema` when there is a
database. They encode the order that works (grep → lsp → blame → the pull
request the commit index names), which a model otherwise rediscovers badly
every session.

Search skips what nobody asked about. A real checkout is overwhelmingly
dependencies and build output — on one repository we measured, 143,690 of
147,852 files were `node_modules` — so `grep` and `find` walk past
`node_modules`, `vendor`, `dist`, `build`, `target`, `.venv` and the rest.
That is not only speed. The result cap used to fill with vendored copies and
minified bundles before the walk ever reached the source: the same search that
returned 140 dependency hits out of 200, topped by a `.js.map`, now returns the
project's own code. Point `path` at one of those directories and it is searched
normally — the list is about what a walk wanders into, not what it is aimed at.

`ls` with no path lists the checkouts **and the other roots** — `mirrors/` for
the issues and pull requests, `documents/` for the stores, `docs/` for the
dumped schemas. A search with no `path` stays in the checkouts, so looking for
a symbol does not come back half pull-request prose, and it says which roots it
skipped so "no matches" never means two different things.

**Labels scope a search to a domain instead of a directory.** Any object can
carry `metadata.labels`, and the same expression works on the CLI and as
`grep`'s `selector`:

```bash
drover get repository -l team=billing
drover get repository -l 'tier!=frontend'
```

> grep for `ChargeIntent` with selector `team=billing`

kubectl's grammar minus the parts nobody uses: `k=v`, `k!=v`, `k`, `!k`,
comma-ANDed. A selector that matches nothing is an error rather than an empty
result — searching zero files and reporting no matches is the most misleading
answer a search tool can give.

`grep` and `read` see the tree as it is now; `git` sees how it got that way —
who changed a line, when a function first appeared, what a file looked like
three releases ago. It shells out to git for reads only: nothing it can do
writes to a checkout or reaches the network.

`lsp` sees what the code *means*. `grep` finds a string; `lsp` finds the
symbol — the one definition, every real use, the type, the callers — for
TypeScript, Go and Java. Give it the identifier's text; it works out the
column itself.

Servers start the first time a question needs one, and are reaped when nobody
has asked for a while, so an engine nobody queries pays nothing for them. They
install themselves into `~/.drover/servers`: TypeScript 7 straight from the
npm registry over HTTPS (a native binary — no Node, no npm), gopls with `go
install`, jdtls from Eclipse. The `servers` operation reports which are ready
and, for one that is not, exactly why. `DROVER_NO_SERVER_INSTALL=1` leaves
drover to whatever is already on the machine.

The file tools are jailed to the checkouts. `..` is rejected, symlinks are
resolved and re-checked, and walks never follow a link out of the tree.

Everything an agent can reach is read-only. There is no tool that writes a
file, no tool that POSTs, and no tool that writes to a database.

## Where things live

```
~/.drover/
  config.yaml               servers, listen address, apply provenance
  activity.db               the activity ledger — every tool call, on this machine
  objects/<Kind>/<name>.yaml the applied documents — the source of truth
  status/                    observed state, kept apart from desired state
  repos/<name>/              the checkouts
  mirrors/<name>/            issues, pull requests, the commit index
  documents/<store>/         document stores — the only writable tree
  docs/schema/<name>.sql     dumped database schemas
  pending/<name>.yaml        what an untrusted .drover.yaml would apply
```

`repos/` is disposable — sync resets it to the remote and nothing is lost.
`documents/` is the truth, and nothing else has a copy. They are separate
trees so no code path that resets a checkout can be pointed at a store by a
bug in name resolution.

Apply persists. Restart the engine and it forgets nothing; delete the yaml you
applied from and the engine still holds what you gave it.

`drover apply` also records the path it read in `config.yaml`, so the next
`drover serve` picks up the same sources without you maintaining a list.
`--no-remember` opts out.

## Commands

```
drover serve                       run the engine (dashboard on a tty)
drover dash                        open the dashboard for a running engine
drover apply -f <file|dir>         apply objects (-f - reads stdin)
drover get <kind> [name] [-l k=v]  list or show objects, filtered by label
drover delete <kind> <name>        remove an object and its checkout
drover sync [name]                 refresh now
drover call <name> [-p k=v]        execute an HTTPRequest
drover query <name> "SELECT ..."   query a SQLConnection
drover health <name>               re-run a health gate
drover review <repository>         show what a repository declares about itself
drover import openapi -f <spec>    turn an OpenAPI spec into documents
drover import bruno -f <dir>       turn a Bruno collection into documents
drover mcp                         stdio MCP bridge
drover forget <path>               drop a path from the apply list
drover upgrade                     install the latest release
```

`--server <url>` points any client command at an engine elsewhere.
`--data-dir` / `DROVER_DATA` moves the data directory.

## Security

drover has **no authentication**, by design. It listens on `127.0.0.1` so only
that machine can reach it. Bind it to a network interface only if you have
thought about who is on that network.

The `/mcp` endpoint validates `Origin`, because a web page you visit can POST
to a localhost port; a request carrying a non-loopback origin is refused.

## Building

```bash
go build ./cmd/drover
go test ./...
```

No cgo, so every target cross-compiles from anywhere:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/drover
```

The browser dashboard is a Vite build committed under `internal/web/dist` and
embedded with `go:embed`, so building drover itself needs no Node. Changing it
does:

```bash
cd web && npm install && npm run build   # rewrites internal/web/dist
go build ./cmd/drover                    # the binary embeds it -- rebuild too
```

That second line is not optional. The assets live inside the binary, so a
`npm run build` alone changes nothing that a running `drover serve` will
serve.

## License

MIT
