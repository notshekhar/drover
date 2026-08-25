# drover — plan: the warehouse

**Status: phases 0-8 are BUILT** (2026-08-25). Phase 9 (`DocSet`) and phase 10
(cross-repository symbols) are not.

What landed, against the build order in §11: the multi-root jail, labels and
selectors, the issues/pull-request mirror with `index/commits.tsv`, the schema
dump, the OpenAPI and Bruno importers, `.drover.yaml` with quarantine and
`drover review`, MCP prompts, ledger hotspots, and `DocumentStore` with
`doc_write` and per-store git history.

Three decisions were taken differently from the plan below, and the plan text
is left as written so the difference is visible:

1. **A bare `grep` stays in the checkouts** rather than sweeping the mirrors
   (§1, and §13 open question 1). It reports which roots it skipped. Still the
   decision most likely to be wrong.
2. **The mirror backfills across syncs** rather than refusing and asking for a
   flag (§2). One run reads at most 20 pages per stream and the cursor only
   advances over completed pages, so the mirror is usable, partially, the whole
   way through. That is better than a refusal and needed no new command.
3. **`PLAN-DOCS.md`'s `docs:` path prefix was not built.** Document stores are a
   `documents/` root instead, because phase 0 had already made roots general
   enough to carry them, and a root costs no new path grammar and no
   colon-in-a-filename trap. Stores get their own root rather than sharing
   `docs/` with the dumped schemas, for the durability reason `PLAN-DOCS.md`
   gives for keeping them out of `repos/`.

One measurement worth keeping, from looking at grep afterwards: on a 154k-file
checkout, a search reads **846 MB, of which 815 MB is binary** it then discards,
and the tracked corpus is **18.3 MB**. `grepFile` now sniffs 8 KB before
reading a whole file. The rest — a literal prefilter before the regexp probe
(measured 6x, since Go's `regexp` runs at 34-88 MB/s and `bytes.Index` at
~500 MB/s) and taking the file list from `git ls-files` — is not built.

`PLAN-OBSERVABILITY.md` is parked, not cancelled — logs and cluster state come
after this. It shares phase 0 with this plan, and nothing else.

---

## 0. The rule that orders all of this

drover's thesis is that real files and real `grep` give exact answers. The
corollary, which this plan takes seriously for the first time:

> **The warehouse grows by materialising sources into the tree, not by adding
> tools.**

Issues become files. PR discussions become files. A database schema becomes a
file. Crawled documentation becomes files. None of those cost a tool, because
`ls` / `read` / `grep` / `find` already reach anything that is on disk inside
the jail.

Counted out: nine features, **one** new tool (`doc_write`, §6), because writing
is the only verb the existing four do not have. Ten tools becomes eleven.

## 1. Phase 0 — the jail grows roots

Today `files.Root` is one directory, `$DROVER_DATA/repos`, and a path an agent
passes is relative to it: `api/src/main.go`. Everything below needs to put
files somewhere that is *not* a checkout — because a checkout is a mirror that
gets `reset --hard` onto the remote, and because issues are not source.

**The move: named roots, with `repos` as the default one.**

```go
type Root struct {
    Dir   string            // $DROVER_DATA/repos, the default root
    Extra map[string]string // "mirrors", "docs", "logs" -> absolute dirs
}
```

- `resolve(rel)` looks at the first path segment. If it names an extra root,
  the path resolves inside that root. Otherwise it resolves inside `repos`,
  exactly as now.
- **Every path that works today still works.** `api/src/main.go` is untouched,
  which matters because those paths are baked into `git`'s prefix trimming,
  `lsp`'s result format, and the tool descriptions the model has learned.
- `ls` with no path lists the repositories **and** the extra roots, so the
  agent discovers `mirrors/` the same way it discovers a repository.
- Both containment checks — lexical `..` and the symlink re-resolve — run
  against whichever root was selected. One jail, several roots, not several
  jails. `Resolve` stays the single entry point `lsp` and `git` already use.

Two consequences that have to be decided here rather than discovered later:

**Reserved names.** A Repository called `mirrors` would shadow a root.
`ValidateName` gains a reserved list (`mirrors`, `docs`, `logs`) and refuses
it at apply, with the reason. Cheaper than a collision at resolve time.

**A walk with no path stays inside `repos`.** This is the important one.
`grep` with no `path` searches the checkouts and *not* the mirrors, because a
search for a function name should not be half PR comments. The result carries
one line saying the other roots exist and were not searched — the same honesty
`skipDirs` already practises. Naming a root explicitly (`path: mirrors/api`)
searches it normally.

## 2. A Repository mirrors its issues and pull requests

`git` says what changed and who changed it. It cannot say **why**, because why
was argued in a pull request and filed in an issue, and neither is in the
clone. This is the largest single gap between drover and "the whole context",
and it costs no new tool.

```yaml
kind: Repository
metadata: {name: api}
spec:
  url: https://github.com/acme/api
  branch: main
  mirror:
    issues: true
    pullRequests: true
    since: 365d          # default; "all" for a full backfill
    state: all           # all | open
    comments: true       # the discussion, not just the body
```

### Where it lives, and why not in the checkout

Under `$DROVER_DATA/mirrors/<repo>/`, reached as `mirrors/api/...`.

Not inside the checkout, even though adjacency would be nice, for two
measured-by-reading reasons: the worktree is a mirror that gets `reset --hard`
onto `FETCH_HEAD` on every sync, and — the subtler one — `upToDate` skips that
reset by asking `status --porcelain`, which reports untracked files. A
`.drover/` directory inside the checkout would make every tick look dirty and
undo the reset-skip optimisation deliberately built in `d6058ea`. It could be
hidden in `.git/info/exclude`, but that is a trick, and tricks in the sync path
are how mirrors start eating data.

```
~/.drover/mirrors/api/
  issues/1234.md
  pulls/5678.md
  index/commits.tsv      sha \t pr-number \t title
  index/by-label.tsv
  cursor.yaml            updated_at high-water mark, per stream
```

### The file format

Markdown with YAML frontmatter, because frontmatter makes structure greppable
without making prose unreadable:

```markdown
---
number: 5678
kind: pull
state: merged
title: rate limit the webhook endpoint
author: someone
created: 2026-03-04T09:12:00Z
merged: 2026-03-06T14:02:11Z
labels: [backend, incident-followup]
commits: [a1b2c3d, e4f5a6b]
files: 7
---

# rate limit the webhook endpoint

<body>

## review: another-person (changes requested) — 2026-03-05T11:00Z
<comment>

## comment: someone — 2026-03-05T11:40Z
<comment>
```

`grep '^state: open' mirrors/api/issues` works. So does grepping the prose.
That is the whole design.

### `index/commits.tsv` is the killer path

Three columns, one flat file, and it turns `git blame` into an answer about
intent:

```
git blame -> a1b2c3d -> grep a1b2c3d mirrors/api/index/commits.tsv -> pull 5678 -> read it
```

Four hops an agent can actually take, none of them a new tool, all of them
exact. This file is the single highest-value artefact in this plan.

### Fetching it

**Shell out to `gh` when it is on PATH**, for the same reason `git`, `go
install` and `kubectl` are shelled out to: it already solves auth, including
enterprise SSO and token refresh, and solving auth again here would be the
whole cost of the feature. Fall back to `${GITHUB_TOKEN}` against the REST API
when `gh` is absent, and say which one is in use in `drover get repository`.

- **Incremental by `updated_at`.** `cursor.yaml` holds the high-water mark per
  stream; a sync fetches what changed since. First sync is a backfill.
- **A backfill is estimated before it runs.** A repository with 40,000 issues
  is 400 API pages and a rate-limit wall, so the first sync reports *"api has
  12,400 issues since 2025-08; that is ~130 requests, run `drover sync api
  --backfill` to do it"* rather than starting it inside a reconcile tick. Same
  instinct as the S3 budget in the other plan: an expensive thing announces
  itself.
- **Rate limits are a state, not an error.** 403 with `X-RateLimit-Remaining:
  0` records the reset time in status and resumes at it. A half-finished
  backfill is resumable because the cursor only advances over completed pages.
- **Deleted upstream is not deleted here.** A closed issue stays. The mirror is
  additive by design; `drover delete repository` takes it all with it.

### Later, not now

GitLab, Linear and Jira are the same shape — fetch, render markdown, index —
behind a `provider:` field. GitHub first, and the file layout is deliberately
provider-neutral so the second one adds a fetcher and nothing else.

## 3. `.drover.yaml` in the repository, and labels

### The repository describes its own context

A service knows which database it talks to, which API it calls, and where its
docs are. Today a human transcribes that into `~/.drover`. Instead: on sync, if
`<checkout>/.drover.yaml` exists, its objects are read.

```yaml
# committed at the root of acme/api
apiVersion: drover/v1
kind: HTTPRequest
metadata: {name: get-user}
spec: {...}
---
apiVersion: drover/v1
kind: DocSet
metadata: {name: runbooks}
spec: {...}
```

**And then it is quarantined, because a repository is remote content.** This is
the security decision of this plan and it is not negotiable: a yaml file inside
a clone is written by whoever can push to that repository, which is not
necessarily the person running the engine. Applying it automatically would mean
a PR to a vendored dependency can point a `SQLConnection` at an attacker's host
or add a `Repository` that clones something enormous.

So:

- Objects from a repository are **parsed, validated, stored as pending, and not
  applied**, until the Repository carries `spec.trustConfig: true`. Until then
  `drover get repository api` shows *"3 pending objects from .drover.yaml — run
  `drover trust api` to review"*, and `drover trust api` prints them and asks.
- **`Repository` and `SQLConnection` are never accepted from a repository, at
  all**, trusted or not. A clone target and a database url are the two things
  that reach the network on drover's credentials. `Environment`, `HTTPRequest`,
  `DocSet` and `DocumentStore` are allowed.
- **Names are prefixed** `api.get-user`, so two repositories cannot collide and
  the origin of an object is visible in its name. This is the one place the
  name grammar gains a dot, and `ValidateName` learns it for this case only.
- An `Environment` from a repository may declare `secrets` as `${ENV}`
  references — that is a reference, not a credential, and the existing rule
  already refuses a literal.

### Labels and selectors

`metadata.labels` on every kind, free-form `map[string]string`, plus one
generated label drover writes itself: `drover.io/source: repository/api` on
anything that came from a `.drover.yaml`.

- `drover get repository -l team=billing`
- `selector:` on `ls`, `grep` and `find` — scope a search to a domain rather
  than to a path. `grep --selector team=billing` searches three checkouts out
  of forty. On a warehouse of any size this is the difference between a search
  and a scan.
- Selector syntax is `k=v`, `k!=v`, `k` (exists), comma-separated as AND. No
  set operators, no `in (...)`. kubectl's grammar minus the parts nobody uses.

## 4. The schema on disk

`sql_query`'s tool description currently tells the model to discover tables via
`information_schema`. That is a wasted round trip at the start of every
session, and the result is thrown away when the session ends.

**Dump it instead**, on health pass and on the health interval, to
`docs/schema/<connection>.sql`:

```sql
-- analytics (postgres 16.3) -- dumped 2026-08-25T10:02:11Z
CREATE TABLE events (
  id          bigint      NOT NULL,
  user_id     bigint      NOT NULL REFERENCES users(id),
  kind        text        NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);  -- ~48,200,000 rows
CREATE INDEX events_user_id_created_at ON events (user_id, created_at DESC);
```

- **DDL-shaped, not a table dump**, because a model reads DDL fluently and it
  greps well: `grep -n 'REFERENCES users' docs/schema` answers "what points at
  users" without a query.
- **Row estimates are included** and are worth more than they cost. A model
  choosing between two join orders, or deciding whether a `SELECT *` is safe,
  needs to know a table is 48 million rows and not 48. From `reltuples` on
  postgres, `information_schema.tables` on mysql, `svv_table_info` on redshift
  — estimates, labelled as estimates.
- **Drift is visible.** The previous dump is kept; the reconciler writes a
  one-line status when they differ. A schema that changed under a running agent
  is worth knowing about.
- `spec.schemas: [public, billing]` allowlists what gets dumped, because a
  warehouse with 10,000 tables should not produce a 40 MB file.

The connection stays read-only and this changes nothing about that: the dump
is three `SELECT`s against catalog views, through the same gate.

## 5. Importers — nobody hand-writes forty request files

```
drover import openapi -f openapi.yaml --prefix github --environment prod
drover import bruno   -f ./collection --prefix acme
drover import postman -f collection.json          # later
```

Generation, not a new runtime path: an importer emits `HTTPRequest` and
`Environment` documents to stdout or applies them. What comes out is ordinary
yaml a human can read, edit and commit.

| OpenAPI | becomes |
|---|---|
| `operationId` | `metadata.name`, kebab-cased |
| `servers[0].url` | an `Environment` variable `baseUrl` |
| path templating `{userId}` | a `pathParams` entry — the syntax already matches |
| `parameters[in=query]` | `queryParams` |
| `summary` / `description` | `spec.description`, which is what `api_list` searches |
| non-GET operations | emitted, stored, **never advertised** — the existing rule |

- **A spec with 400 operations is a refusal, not 400 files.** `--tag`,
  `--filter` and a dry-run count come first: *"this spec has 412 operations;
  narrow with --tag billing or pass --all"*.
- **Bruno is already solved.** `bruh` has a Go `.bru` importer verified at
  223/223 on a real collection. Port it rather than writing a second one, and
  keep the parser in one place if it can reasonably be shared.
- Vendor extensions and `$ref` cycles are the two things that break OpenAPI
  parsers. Local `$ref` is resolved, remote `$ref` is refused with the reason
  (it is a network fetch on an untrusted url), and unknown `x-` keys are
  ignored.

## 6. DocumentStore — the only write path

`PLAN-DOCS.md` designed this and parked it. Unshelve it as designed: the store
is an object, the documents are files, no tool applies document yaml. Reached
as `docs/<store>/...` through the phase-0 root.

Two additions to what that document says.

**Every store is a local git repository.** `git init` on create, and every
agent write is a commit whose author is the attribution the activity ledger
already resolves (`claude-code 2.1.4 over stdio`) and whose message is the
`reason` argument. That buys three things for one `exec.Command`: an agent
cannot silently destroy a document, "who wrote this and why" is answerable with
the `git` tool that already exists, and `git log docs/product` is a changelog
nobody had to build. It is a *local* repository with no remote — drover never
pushes.

**One new tool, `doc_write`**, and it is the only tool in drover that writes.
It carries the weight accordingly:

- Refused unless the store has `spec.writable: true`.
- Jailed to that store's root, no `..`, no symlink escape — the same
  `Root.Resolve`.
- Markdown only, size-capped, and the path must be inside the store's declared
  layout.
- Every call recorded in the ledger with the diff stat, and every call a
  commit.
- Reads go through `read` and `grep` like everything else. There is no
  `doc_read`.

## 7. What the ledger already knows

`activity.db` records every tool call, and nothing reads it back except the
dashboard. It is an index of what agents actually needed, built from behaviour
rather than from embeddings, and it costs one query.

- `drover get hotspots` and a line in the MCP handshake: *"most-read files in
  api: internal/db.go, cmd/serve.go, internal/auth/session.go"*.
- **Stated as observation, never as recommendation.** "Agents most often read"
  is a fact about the past; "you should read" is advice drover has not earned.
  The phrasing matters because a model will treat a recommendation as a
  shortcut and stop looking.
- **Capped and decayed**, top five per repository over a trailing window, or
  the list becomes a feedback loop in which the files that were read stay read.
- Also cheap and worth having: the searches that returned nothing. A recurring
  empty grep is usually a repository that should be in the warehouse and is
  not.

## 8. Prompts, because `prompts/list` returns `[]`

MCP has a prompt surface. drover answers it with an empty array today, which
is a whole standard channel left on the floor.

Generated from what the engine holds, so they are never stale:

| prompt | arguments | expands to |
|---|---|---|
| `onboard` | repository | a walk-through: what the repo is, its entry points, its hot files (§7), its `.drover.yaml` objects |
| `investigate` | repository, symptom | grep → lsp → git blame → the PR from `index/commits.tsv` (§2), in that order |
| `schema` | connection | the dumped schema plus the queries worth running first |

A prompt is not a tool and does not touch the fixed-ten count.

## 9. `kind: DocSet`

The mirror model, pointed at documentation instead of a git remote.

```yaml
kind: DocSet
metadata: {name: stripe}
spec:
  seeds: ["https://docs.stripe.com/api"]
  allow: ["https://docs.stripe.com/api/"]   # prefix allowlist, required
  depth: 3
  refreshInterval: 7d
  budget: 200MiB
```

Crawl → HTML to markdown → one file per page under `docs/stripe/`, plus
`index.md` mapping url to path. Then `grep` answers questions about the Stripe
API without a single WebFetch, and re-answers them for free.

- **`allow` is required and is a prefix list.** A crawler with no boundary is a
  way to make drover fetch arbitrary urls; this is the same instinct as
  refusing a remote `$ref` in §5.
- robots.txt respected, one request at a time per host, budget enforced.
- **A JavaScript-rendered site is reported, not written.** A crawl that
  produces pages of empty shells is worse than no crawl, because grep then
  confidently returns nothing. If the extracted text is below a threshold on
  the first few pages, the DocSet fails with "this site renders client-side"
  and stores nothing.

## 10. Cross-repository symbols

`lsp workspace_symbols` needs a repository today. Without one, fan out across
every repository whose server is already running, plus any that can start
inside a deadline, merge and cap. A warehouse-wide "where is `ChargeIntent`
defined, anywhere" is a question only drover can answer, and it is the natural
end of having many checkouts in one place.

Cheap half first: a per-repository symbol cache file written after a successful
`document_symbols` sweep, so the common lookup does not start four language
servers.

## 11. Build order

| phase | what | done when |
|---|---|---|
| 0 | multi-root jail, reserved names, walk scope | every existing path still resolves; `ls` shows the roots; jail tests pass unchanged |
| 1 | labels + selectors | `drover get -l` and `grep --selector` work |
| 2 | issues/PR mirror, `index/commits.tsv` | blame → sha → PR → discussion, on a real repository |
| 3 | schema dump | a real postgres schema on disk with row estimates |
| 4 | OpenAPI + Bruno import | a real spec becomes usable requests |
| 5 | `.drover.yaml` + quarantine + `drover trust` | an untrusted repo's objects are visible and inert |
| 6 | prompts | three prompts listed and expanded |
| 7 | ledger hotspots in the handshake | — |
| 8 | DocumentStore + `doc_write` + per-store git | a write is a commit with attribution |
| 9 | DocSet | a real docs site greppable offline |
| 10 | cross-repo symbols | — |

Phase 1 before phase 2 because the mirror wants a label to scope by, and the
selector is small.

## 12. Traps, predicted

- **`status --porcelain` sees untracked files**, which is why mirrored content
  is not in the checkout (§2). Putting it there silently disables the
  reset-skip.
- **GitHub's `updated_at` is not monotonic across pages.** Sorting by
  `updated` while paginating a stream that is still being updated can skip an
  item. Sort by `created` for backfill and use `updated` only for the delta,
  with an overlap window.
- **`gh` writes to stderr and exits 0** on partial failures; check the JSON, not
  the exit code.
- **A PR body can contain `---`.** The frontmatter writer must escape or fence
  it, or the file stops parsing as frontmatter — exactly the failure mode
  `PLAN-DOCS.md` predicted for documents-in-yaml, arriving here instead.
- **`information_schema` is slow on a large postgres**, minutes on a warehouse.
  The dump runs on the health schedule, not in a request path, and carries its
  own timeout.
- **OpenAPI `$ref` cycles** are common in generated specs and will hang a naive
  resolver.
- **A repository can contain a `.drover.yaml` that is 200 MB.** Size-cap the
  read before parsing.
- **Reserved names must be checked at apply and at load**, or an existing
  object named `docs` from before this change breaks resolution after an
  upgrade. The store loader needs the same check with a clear migration error.

## 13. Still open

1. **Should `grep` with no path search the mirrors?** Decided no (§1), and it
   is the decision most likely to be wrong. The counter-argument is real: an
   agent asking "why is this rate limited" wants the PR, and will not think to
   pass `path: mirrors/api`. A middle option exists — search `repos` first and,
   only when there are no hits at all, say "no hits in the code; `mirrors/api`
   has 12" without returning them. Worth trying once the mirror exists.
2. **Does `.drover.yaml` need a signature?** Trust is per-repository and
   coarse: trusting `api` trusts whoever can push to `api` forever. A signed
   file, or a hash pinned in the Repository spec, would make trust specific to
   a version. Probably over-engineering for the first cut, and definitely the
   right answer eventually.
3. **Does the mirror belong under `mirrors/` or under `docs/`?** They are both
   "prose about the code". Kept separate because one is fetched and immutable
   and the other is written and versioned, and merging them would put an
   agent's writes next to a mirror that overwrites.
4. **What happens to a DocumentStore when two agents write the same file?**
   Last-write-wins with both commits in history is the cheap answer and is
   probably right, but nobody has thought about it for more than a minute.
