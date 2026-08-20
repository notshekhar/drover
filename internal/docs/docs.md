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
| `grep` | regular-expression search across the checkouts |
| `find` | find files by name or path pattern |
| `api_list` | find a request, with a fuzzy search over everything a request says; also lists the environments |
| `api_describe` | one request's parameters in full |
| `api_call` | perform a request |
| `sql_query` | a read-only query against a named connection |

The tool set is **fixed at eight** however much you add. Twenty requests do
not become twenty tools — they become entries `api_list` returns, and the
databases are listed inside `sql_query`'s own description.

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
