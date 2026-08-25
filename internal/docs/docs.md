# drover — how to add things

This file is for you, or for an AI agent you point at it. It explains
everything drover can hold and exactly how to write it down.

**Drop `.yaml` files in this folder and drover applies them.** That is the
whole workflow. Any `*.yaml` or `*.yml` file sitting directly in
`~/.drover/` is read on startup and whenever the engine reloads.

```
~/.drover/
  docs.md              ← you are here
  config.yaml          ← drover's own settings; NOT an object file
  repos.yaml           ← anything else you drop here gets applied
  github-api.yaml
  databases.yaml
  objects/             ← drover's internal store; do not edit by hand
  repos/               ← the checkouts; do not edit by hand
  status/              ← observed state; do not edit by hand
```

Pick any filenames you like. One file per topic reads well
(`repos.yaml`, `github-api.yaml`), and one file can hold many objects
separated by `---`.

After writing a file:

```bash
drover apply -f ~/.drover/repos.yaml   # apply it now
```

You do not have to do even that: drover watches this folder, so saving a file
is enough. `drover apply -f` is for checking a file's errors before you rely
on it.

Check what landed:

```bash
drover get repository
drover get environment
drover get httprequest
drover get sqlconnection
```

---

## The rules that apply to everything

Every document has the same envelope:

```yaml
apiVersion: drover/v1
kind: <Repository | Environment | HTTPRequest | SQLConnection>
metadata:
  name: <name>
  labels:            # optional
    team: billing
spec:
  ...
```

- **Kinds are spelled out in full.** `Repository`, never `Repo`.
  `HTTPRequest`, never `Curl`. `SQLConnection`, never `SQL`.
- **`metadata.name` is lowercase** letters, digits and dashes, up to 63
  characters, not starting or ending with a dash. `api-server` is fine;
  `API_Server` is not. The name becomes a directory, and uppercase would
  collide with lowercase on macOS.
- **Two objects of the same kind cannot share a name.** If two documents in
  one apply use the same name, the whole apply is rejected and both files are
  named in the error. Re-applying an existing name *updates* that object,
  which is normal and fine.
- The same name across different kinds is fine: `Repository/api` and
  `HTTPRequest/api` are different objects.
- **An unknown field is an error**, not a warning. A typo like
  `refreshIntervl` fails the apply rather than being silently ignored.
- Apply is all-or-nothing. One bad document means nothing is written.
- **Names may not be `mirrors`, `docs` or `logs`.** Those are top-level
  directories the file tools use for things that are not checkouts, so a
  repository by one of those names would shadow one.

### Labels

`metadata.labels` is an optional map on any object. Keys and values are
lowercase letters, digits, dashes and underscores; a key may carry a prefix
(`app.kubernetes.io/part-of`). The `drover.io/` prefix is reserved for labels
drover writes itself.

Labels are what keep a warehouse of forty checkouts navigable:

```bash
drover get repository -l team=billing
drover get repository -l tier!=frontend
drover get repository -l owner            # the label exists
drover get repository -l '!owner'         # it does not
```

Clauses are comma-separated and ANDed: `-l team=billing,tier=backend`. There
is no OR and there are no set operators.

The same expression works as `selector` on the `grep` and `find` tools, which
is how an agent searches a domain rather than a directory:

> grep for `ChargeIntent` with selector `team=billing`

A selector and a `path` cannot be given together — the selector already says
which checkouts to search.

---

## Repository — a git checkout

drover clones the repository and keeps it fresh. Agents get `ls`, `read`,
`grep` and `find` over it.

```yaml
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
  refreshInterval: 15m
```

| field | required | notes |
|---|---|---|
| `url` | yes | `https://`, `ssh://`, `git@host:path`, or an absolute local path |
| `branch` | yes | drover clones one branch; there is no default |
| `refreshInterval` | no | how often to pull. Minimum `30s`. `never` disables the timer. Omitted means the server default (1h) |

Several repositories in one file:

```yaml
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
  refreshInterval: 15m
---
apiVersion: drover/v1
kind: Repository
metadata:
  name: web
spec:
  url: https://github.com/acme/web
  branch: main
  refreshInterval: 1h
---
apiVersion: drover/v1
kind: Repository
metadata:
  name: vendored-docs
spec:
  url: https://github.com/someone/docs
  branch: main
  refreshInterval: never    # a reference that never changes
```

**Private repositories** use whatever credentials the engine's own
environment has — `GITHUB_TOKEN`, `gh auth`, or an ssh key. Never put a
token in the yaml.

**The checkout is a mirror.** Every sync resets it to the remote, so do not
edit files in `~/.drover/repos/`. drover refuses to touch a directory it did
not create, so it cannot destroy a checkout of your own that happens to share
a name.

---

### trustConfig — letting a repository describe itself

A service knows which API it calls and where its docs are. Rather than
transcribe that into `~/.drover` by hand, commit a **`.drover.yaml`** at the
repository root:

```yaml
# committed in acme/api
apiVersion: drover/v1
kind: Environment
metadata: {name: prod}
spec:
  variables: {baseUrl: https://api.acme.com}
---
apiVersion: drover/v1
kind: HTTPRequest
metadata: {name: get-user}
spec: {...}
```

**It is not applied until you say so.** A yaml file inside a clone is written
by whoever can push to that repository, which is not necessarily you. So
drover parses it, shows it, and leaves it inert:

```bash
drover review api        # prints exactly what would be applied
```

If you have read it and want it:

```yaml
kind: Repository
spec:
  trustConfig: true
```

There is no `drover trust` command that flips a hidden switch — trust is a
line in the document you already control, so it lives where the rest of your
desired state lives.

Rules that hold whether or not you trust it:

- **`Repository` and `SQLConnection` are never accepted from a repository.** A
  clone target and a database url are the two things that reach the network on
  drover's own credentials.
- **Names are namespaced.** `get-user` becomes `api.get-user`, so two
  repositories cannot collide, and an environment reference inside the file is
  namespaced with it.
- **drover labels what it took**: `drover.io/source: repository/api`. The
  `drover.io/` prefix is reserved, so a document cannot forge it — list what a
  repository contributed with
  `drover get httprequest -l drover.io/source=repository/api`.
- The file is size-capped before it is parsed, and a malformed one is
  reported against the config, never against the checkout.

### mirror — the discussion beside the code

`git` says what changed and who changed it. It cannot say **why**, because why
was argued in a pull request and filed in an issue, and neither is in the
clone. Ask drover to mirror them:

```yaml
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
  mirror:
    issues: true
    pullRequests: true
    since: 365d        # 90d, 6h, or all. Default 365d.
    state: all         # all | open. Default all.
    comments: true     # the thread, not just the opening body. Default true.
```

The result is markdown on disk, one file per item, reached by an agent as
`mirrors/api/...`:

```
mirrors/api/
  issues/1234.md         frontmatter + body + the whole thread
  pulls/5678.md
  index/commits.tsv      sha -> pull request number
  cursor.yaml            how far it has read; delete to re-mirror
```

Each file opens with a YAML header, so structure is greppable without making
the prose unreadable:

```markdown
---
number: 5678
kind: pull
state: merged
title: rate limit the webhook endpoint
author: someone
labels: [backend, incident-followup]
---
```

`grep '^state: open' mirrors/api/issues` works. So does grepping the argument.

**`index/commits.tsv` is the reason this exists.** It maps a commit to the
pull request that carried it, built from the checkout's own history at no API
cost, so an agent can go:

```
git blame  ->  a1b2c3d  ->  grep a1b2c3d mirrors/api/index/commits.tsv  ->  pull 5678  ->  read it
```

Four hops from a line of code to the argument about it.

**Authentication.** drover uses `gh` if it is on PATH, which means enterprise
SSO and token refresh already work. Otherwise it uses `GITHUB_TOKEN` or
`GH_TOKEN`. A public repository works with neither, at a lower rate limit.
`drover get repository` says which one is in use.

**A big repository backfills over several syncs.** One run reads at most 20
pages per stream, records how far it got, and continues on the next tick — so
the mirror is usable, partially, the whole way through rather than stalling a
sync for minutes. A rate limit is recorded with the time it resets and is not
treated as a failure.

**A mirror failure is never a repository failure.** GitHub being unreachable
does not make the checkout less searchable, so it is reported on its own line
and `STATUS` stays `ready`.

**Searches stay in the code by default.** `grep` with no `path` searches the
checkouts, not the mirrors — a search for a function name should not come back
half pull-request prose. The result says which roots it skipped. Pass
`path: mirrors/api` to search the discussion.

Only GitHub, for now.

---

## Environment — named stages for API calls

An Environment is a bag of values that HTTP requests fill their placeholders
from. Write one per stage so a request is written once instead of three
times.

```yaml
apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
spec:
  description: production
  variables:
    baseUrl: https://api.acme.com
    tenant: acme
  secrets:
    token: ${ACME_PROD_TOKEN}
---
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

- **`variables`** are plain values. Safe to read, safe to show an agent.
- **`secrets`** must be a bare `${ENV_VAR}` reference and nothing else. A
  literal value is rejected, and so is a partial one like
  `"Bearer ${TOKEN}"` — put the `Bearer ` prefix in the request that uses it.
  This is what stops a credential ending up in a file someone commits.
- Secret values are read from the **engine's** environment, so set them
  wherever `drover serve` runs, then restart it. (Editing yaml is picked up
  automatically; a new environment variable is not, because the engine's own
  environment is fixed when it starts.) Their values are never
  printed, never logged, and never shown to an agent.
- A secret whose variable is unset does not fail the apply. It fails when a
  request that needs it is actually called, and says which variable is
  missing. The `drover serve` dashboard flags unset secrets.

---

## HTTPRequest — one callable API request

A collection of API calls is just several `HTTPRequest` documents in one
file, usually alongside the `Environment` they share.

```yaml
apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  description: Fetch one user by id. Use when you already have a user id.
  method: GET
  url: "{{baseUrl}}/v1/users/{userId}"

  environments: [local, prod]
  defaultEnvironment: prod

  pathParams:
    - name: userId
      description: Opaque user id, looks like usr_1a2b3c
      required: true
      example: usr_1a2b3c

  query:
    - name: include
      description: Comma-separated relations to expand
      required: false
      example: profile,billing

  headers:
    - name: Authorization
      value: "Bearer {{token}}"
    - name: Accept
      value: application/json
```

Run it:

```bash
drover call get-user -p userId=usr_1a2b3c --environment prod
```

### The three placeholder syntaxes

They are deliberately different, because they come from different places:

| syntax | filled from | can a caller set it? |
|---|---|---|
| `{{name}}` | the selected Environment's variables and secrets | **no** |
| `${NAME}` | the engine's own process environment | **no** |
| `{name}` | a parameter you declared in `pathParams` or `query` | **yes** |

Only `{name}` ever becomes a tool parameter, so an agent can never reach a
secret by asking for it as an argument.

### Rules that will reject your document

- Every `{name}` in the url **must** have a matching entry in `pathParams`,
  and every `pathParams` entry must appear in the url. Both directions are
  checked, so a tool never advertises a parameter that goes nowhere.
- Every parameter **needs a `description`**. It is what an agent reads to
  decide what to put there, so an empty one makes the tool unusable.
- **Headers may not use `{name}`.** They can use `{{env}}` and `${PROC}`
  only. A caller-settable header is how credentials get stolen.
- `defaultEnvironment` must be one of `environments`.
- Every name in `environments` must exist as an `Environment` object. Apply
  the Environment in the same file or before the request.
- The url must be absolute after substitution, and must be http or https.
  Starting it with `{{baseUrl}}` is the normal way.

### About methods

**Agents are only ever offered GET.** You may store a POST, PUT or DELETE
and run it yourself with `drover call <name> --allow-write`, but it will
never be advertised as a tool. In the dashboard those show a hollow `○`.

### Importing a collection you already have

Nobody hand-writes forty request files. If there is an OpenAPI spec or a Bruno
collection, convert it:

```bash
drover import openapi -f openapi.yaml --prefix github --environment prod
drover import bruno   -f ./my-collection --prefix acme
```

It prints yaml to stdout. That is deliberate: what comes out is an ordinary
document you can read, edit and commit, and an importer's guesses are the part
worth looking at. Write it somewhere with `--out billing.yaml`, or pass
`--apply` once you trust it.

- `--tag billing` narrows an OpenAPI import to one tag.
- A spec with more than 40 operations **refuses** and tells you the count.
  Narrow it, or pass `--all`.
- Operations drover cannot express are skipped with the reason on stderr —
  most often a `header` parameter, which drover will not let a caller set.
- Missing descriptions are derived from whatever the spec did say (type,
  enum, default), because a parameter with no description is refused at apply.
- Non-GET operations are imported and stored; drover's normal rule still
  applies, so they are never advertised to an agent.

### A whole collection in one file

```yaml
apiVersion: drover/v1
kind: Environment
metadata:
  name: github
spec:
  variables:
    baseUrl: https://api.github.com
  secrets:
    token: ${GITHUB_TOKEN}
---
apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-repo
spec:
  description: Fetch one GitHub repository by owner and name.
  method: GET
  url: "{{baseUrl}}/repos/{owner}/{repo}"
  environments: [github]
  defaultEnvironment: github
  pathParams:
    - name: owner
      description: The org or user that owns it, like golang
      required: true
      example: golang
    - name: repo
      description: The repository name, like go
      required: true
      example: go
  headers:
    - name: Authorization
      value: "Bearer {{token}}"
---
apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: list-issues
spec:
  description: List open issues on a repository, newest first.
  method: GET
  url: "{{baseUrl}}/repos/{owner}/{repo}/issues"
  environments: [github]
  defaultEnvironment: github
  pathParams:
    - name: owner
      description: The org or user that owns it
      required: true
    - name: repo
      description: The repository name
      required: true
  query:
    - name: state
      description: open, closed or all
      required: false
      example: open
  headers:
    - name: Authorization
      value: "Bearer {{token}}"
```

---

## DocumentStore — the one place an agent can write

Everything else drover offers is read-only: GET requests, read-only SQL, file
tools with no write. That leaves a gap — the warehouse has nowhere to put what
an agent worked out, so every session starts from nothing. A document store is
that place, and it is deliberately the narrowest possible exception.

```yaml
apiVersion: drover/v1
kind: DocumentStore
metadata:
  name: product
spec:
  description: PRDs, TRDs and decision records for the billing platform.
  path: /Users/you/work/product-docs   # optional; defaults into the data dir
  writable: true                       # default true
  history: true                        # default true; see below
```

`description` is not decoration: it goes in the connection inventory and in
the write tool's catalogue, so a model is told *product is where PRDs live*
rather than guessing from the name.

An agent reaches it at `documents/<store>/...`:

```
documents/product/prd-billing.md
documents/product/decisions/0001-why-postgres.md
```

Reads go through `read` and `grep` like everything else — there is no
`doc_read`. Writes go through the one write tool:

```
doc_write  store=product  path=decisions/0001-why-postgres.md
           content="# Why postgres\n..."  reason="record the decision"
```

Rules, all of them deliberate:

- **Markdown only.** A store holds prose that `grep` can read; a binary blob
  in there is a result nobody can read and a file nobody can review.
- **A write replaces the whole file.** `read` it first if you mean to edit.
- **Every store is a local git repository.** Each write is a commit whose
  author is the agent's attribution and whose message is its stated reason, so
  "who wrote this and why" is answerable with the `git` tool that already
  exists, and nothing an agent writes is unrecoverable. It has no remote and
  drover never pushes. Set `history: false` to turn it off.
- **An identical write is not a commit.** Rewriting the same bytes would fill
  the history with nothing, and the history is the reason it is worth keeping.
- **`writable: false`** makes a store an agent can read and cannot change,
  which is what you want for somebody else's documents.
- **A store with its own `spec.path` is never deleted.** `drover delete`
  stops drover offering it; it does not destroy a directory drover did not
  create.

`documents/` and `repos/` are separate trees on disk for the reason that
matters: **they have opposite durability rules.** A checkout is disposable —
sync resets it to the remote and nothing is lost. A store is the truth;
nothing else has a copy.

---

## SQLConnection — a database an agent can query

```yaml
apiVersion: drover/v1
kind: SQLConnection
metadata:
  name: analytics
spec:
  description: the analytics warehouse
  provider: postgres
  url: ${ANALYTICS_DATABASE_URL}
  health: SELECT 1
  maxRows: 200
```

| field | required | notes |
|---|---|---|
| `provider` | when `url` is a `${ENV}` reference | `postgres`, `mysql` or `redshift` |
| `url` | yes | use a `${ENV_VAR}` reference. A literal is only accepted when it carries no password |
| `health` | for the tool to appear | a read-only query that proves the connection works |
| `readOnly` | no | defaults to **true** |
| `maxRows` | no | defaults to 200 |
| `timeoutSeconds` | no | per query |

Run one:

```bash
drover query analytics "SELECT count(*) FROM events"
```

### The three providers

```yaml
spec:
  provider: postgres
  url: ${DATABASE_URL}          # postgres://user@host:5432/dbname
---
spec:
  provider: mysql
  url: ${MYSQL_URL}             # mysql://user@host:3306/dbname
---
spec:
  provider: redshift
  url: ${REDSHIFT_URL}          # redshift://user@cluster...:5439/dev
```

Redshift speaks the PostgreSQL wire protocol but its SQL dialect is older and
more limited, so it is its own provider and agents are told which dialect they
are writing for.

### Read-only is the default, and it is enforced

Only `SELECT`, `WITH`, `SHOW`, `EXPLAIN`, `DESCRIBE`, `VALUES` and `TABLE` are
allowed. Writes are refused. So are two statements in one call, and a leading
comment cannot disguise a write. Anything unrecognised is refused rather than
assumed harmless.

To allow writes you must say so deliberately:

```yaml
spec:
  readOnly: false
```

Think hard before doing that on a database an agent can reach.

### The schema lands on disk

When the health gate passes, drover dumps the database's shape to
**`docs/schema/<name>.sql`** — every table, column, foreign key, index and
row-count estimate, as DDL:

```sql
CREATE TABLE events (
  id          bigint      NOT NULL,
  user_id     bigint      NOT NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now()
);  -- ~48,200,000 rows
CREATE INDEX events_user_id_created_at ON events (user_id, created_at DESC);
```

An agent reads or greps that file instead of spending its first query on
`information_schema`. `grep 'REFERENCES users' docs/schema` answers "what
points at users" without opening a connection at all.

Row counts are **estimates** from the database's own statistics — good enough
to choose a join order or refuse a `SELECT *`, wrong enough that they should
never be reported as fact. The file says so at the top.

Narrow it on a warehouse with thousands of tables:

```yaml
spec:
  schemas: [public, billing]
```

The dump is refreshed at most once an hour, runs through the same read-only
path a query does, and a dump that fails never fails the health check — the
connection is still queryable, it is just less convenient.

### The health gate

`health` decides whether the tool exists at all. No health query, or one that
fails, and **no `sql` tool is offered** for that connection. Re-check it with:

```bash
drover health analytics
```

Credentials come from the engine's environment, so set `DATABASE_URL` (or
whatever you referenced) where `drover serve` runs.

---

## Giving it all to an agent

```bash
claude mcp add --transport http drover http://127.0.0.1:7432/mcp
```

The agent then has:

| tool | what it does |
|---|---|
| `ls` | list a directory; no path lists the repositories |
| `read` | read a file, with line numbers |
| `grep` | regular-expression search across the checkouts, skipping dependency and build directories |
| `find` | find files by name or path pattern, skipping dependency and build directories |
| `git` | history: `log`, `show`, `diff`, `blame`, `search`, `file`, `branches`, `tags`, `contributors`, `status` |
| `lsp` | code by meaning: `definition`, `references`, `hover`, `implementations`, `document_symbols`, `workspace_symbols`, `incoming_calls`, `outgoing_calls`, `diagnostics`, `servers` |
| `api_list` | find a request, with a fuzzy search over everything a request says; also lists the environments |
| `api_describe` | one request's parameters in full |
| `api_call` | perform a request |
| `sql_query` | a read-only query against a named connection |

The tool set is **fixed at ten** however much you add. Twenty requests do
not become twenty tools — they become entries `api_list` returns, the
databases are listed inside `sql_query`'s own description, and `git`'s ten
operations are an `operation` argument rather than ten more tools.

`git` answers the questions the file tools cannot. `grep` and `read` see the
tree as it is now; `git` sees how it got that way. A good sequence for "why
is this code like this" is `grep` to find the line, `git blame` on that file,
then `git show` on the commit blame names.

`lsp` answers the other half: what the code *means*. `grep` finds a string,
`lsp` finds the symbol.

A language server is only started when a question needs one, and is reaped
once nobody has asked it anything for a while, so the tool costs nothing until
it is used. Servers install themselves into `~/.drover/servers` — TypeScript 7
from the npm registry (a native binary, so no Node is involved), gopls via `go
install`, jdtls from Eclipse — and the `servers` operation reports which are
ready and, for one that is not, exactly why. Set
`DROVER_NO_SERVER_INSTALL=1` to leave drover to whatever is already on the
machine.

Positions take the identifier's text rather than a column: `{"operation":
"references", "path": "api/internal/db.go", "symbol": "Connect"}`.

Everything an agent can reach is read-only. There is no tool that writes a
file, none that POSTs, and none that writes to a database.

---

## Keeping drover current

```bash
drover upgrade           # install the latest release
drover upgrade --check   # just look
```

It re-runs the published installer, which verifies the download's checksum
before replacing anything. Your data here is never touched. Restart any
running `drover serve` afterwards to pick up the new binary.

---

## If something does not apply

drover refuses to start rather than run with a file it could not understand,
and the error names the file and the document. Common causes:

- a kind written short (`Repo` instead of `Repository`)
- an uppercase or underscored `metadata.name`
- two objects of one kind sharing a name
- a misspelled field
- a `{param}` in a url with no matching `pathParams` entry, or the reverse
- an `HTTPRequest` naming an `Environment` that does not exist yet
- a literal value in `secrets:` instead of a `${ENV_VAR}` reference

Check a file before restarting anything:

```bash
drover apply -f ~/.drover/whatever.yaml
```

The message says what is wrong and where.
