# drover — plan: document stores (shelved)

**Status: BUILT 2026-08-25**, with one deviation. The `docs:` path prefix this
document designs was not built: `internal/files` had already grown general
named roots for the mirrors, so a store is a `documents/<name>` root instead.
That costs no new path grammar, no colon-in-a-filename trap, and it keeps the
separate-namespaces property this document argues for. Everything else here was
built as designed -- the store is an object, the documents are files, and there
is no tool that applies document yaml. Added beyond it: each store is a local
git repository, so every agent write is a commit with attribution and a stated
reason.

The original status line follows.

**Status: not built, and deliberately so.** The two MCP fixes this document
was originally written alongside — an inventory of what the engine holds in
the initialize handshake, and `drover://reference` / `drover://inventory` as
resources — are **shipped**. See `internal/mcp/inventory.go`,
`internal/mcp/resources.go` and `internal/mcp/notify.go`.

What is left here is the third idea: letting an agent read *and write* PRDs,
TRDs and decision records through the same tools it already uses on the
checkouts. The design below was worked through to the point where it could be
built directly, and then parked. It is kept because the reasoning is the
expensive part — particularly why documents must not be YAML objects, and why
the two kinds get separate path namespaces rather than one.

---

## 3. Document stores

The question that started this: *should documents be a kind, applied as yaml?*

**Half right.** The right half is that the **store** is declared in yaml and
applied like every other object — one model, one place, `drover get
documentstore` works, `~/.drover/objects/DocumentStore/product.yaml` persists
across restarts. The wrong half is putting the **documents** in yaml.

Five reasons documents-as-objects fails:

1. **Stored objects are a re-marshal, not a copy.** PLAN.md says so outright:
   "a YAML re-marshal with a provenance header, not a byte-for-byte copy of the
   applied file". A PRD would come back with its block scalars reflowed and its
   trailing whitespace gone. A document that does not round-trip byte-for-byte
   is not a document store.
2. **Markdown inside YAML is a minefield.** Block scalars, indentation that
   must be stripped exactly, and any `---` in the prose — a horizontal rule, a
   frontmatter block quoted in an example — reading as a document separator.
3. **There is nothing to reconcile.** Every kind drover has is desired state it
   converges something onto: a checkout onto a ref, a pool onto a URL. A
   document is content. Reconciling it means nothing.
4. **Flat lowercase `(kind, name)` cannot express a tree.** Documents want
   `docs:product/prd-billing.md` and `docs:product/decisions/0001-why.md`. The
   naming rules that keep two repositories from fighting over one clone
   directory actively get in the way here.
5. **It breaks the thesis.** drover's pitch is that real `grep` over real files
   is exact. Grep a document wrapped in YAML and every match comes back
   indented, inside a quoted scalar, with the wrapper in the line. The answer
   stops being exact at the moment it is wrapped.

So: **the store is an object, the documents are files.** And therefore **no
tool to apply document yaml** — the agent writes markdown, the store already
exists.

### The kind

```yaml
apiVersion: drover/v1
kind: DocumentStore
metadata:
  name: product
spec:
  description: PRDs, TRDs and decision records for the billing platform.
  path: ~/work/product-docs     # optional; defaults into the data dir
  writable: true                # default true — a store exists to be written
```

With no `path`, documents live at `~/.drover/documents/<name>/`, which is the
zero-config case: apply four lines, start writing. With a `path`, the store
points at a directory you already have — a docs folder in a repo you keep
locally, a synced folder, anything.

`description` earns its place: it goes in the initialize inventory (already built), so the model
is told *product is where PRDs live* rather than inferring it from the name.

### Where the documents actually live

Alongside the checkouts, as a sibling directory in the data dir:

```
~/.drover/
  config.yaml                        drover's own settings
  docs.md                            the reference, for a human to read
  objects/
    Repository/api.yaml
    DocumentStore/product.yaml       the store's declaration — four lines
  status/                            observed state, kept apart
  repos/
    api/                             a mirror; reset to the remote on sync
    web/
  documents/
    product/                         a store; drover never resets this
      prd-billing.md
      prd-onboarding.md
      decisions/
        0001-why-postgres.md
    architecture/
      trd-auth.md
```

`repos/` and `documents/` are siblings on disk for the reason that matters:
**they have opposite durability rules.** A checkout is disposable — sync resets
it to the remote and nothing is lost, because the remote is the truth. A
document store is the truth; nothing else has a copy. Keeping them in separate
trees means no code path that resets a checkout can ever be pointed at a store
by a bug in name resolution.

The declaration and the content are also kept apart. `objects/DocumentStore/
product.yaml` is four lines of desired state that drover re-marshals freely;
`documents/product/**` is content it writes byte-for-byte and never reformats.
That is the same split as `status/` being stored outside the object.

With `spec.path` set, only the last line changes — the content lives where you
pointed it, `~/work/product-docs/prd-billing.md`, and `documents/` holds
nothing for that store. Everything below is identical either way.

### How an agent sees it

The store name is a **root**, exactly like a repository name, and the agent
only ever sees that name — never the absolute path. Same as today: it reads
`api/src/main.go`, not `~/.drover/repos/api/src/main.go`.

```
docs:product/prd-billing.md         →  ~/.drover/documents/product/prd-billing.md
docs:product/decisions/0001-why.md  →  ~/.drover/documents/product/decisions/0001-why.md
```

and with `path: ~/work/product-docs`:

```
docs:product/prd-billing.md         →  ~/work/product-docs/prd-billing.md
```

The mapping is the store's business. An agent that learned the real path could
ask another tool for it, which is the reason the display path exists at all.

### How it is revealed

Four places, no new discovery mechanism in any of them:

1. **`ls` with no path** — the roots, labelled by what they are and whether
   they can be written:

   ```
   api                  repository       main   b045e541
   web                  repository       main   99aa88bb
   vendored-docs        repository       main   4c1e0a72
   docs:product         document store   writable    41 documents
   docs:architecture    document store   writable    12 documents
   ```

   Listing them in the exact form a path takes is the whole teaching mechanism
   for the prefix — a model that has seen this output does not need to be told
   the rule twice. The label is load-bearing for the same reason: without it a
   model tries to write into `api`, gets refused, and burns a turn learning
   what the listing could have said.

2. **The inventory in `instructions`** (already built) — named, with their descriptions,
   before the first tool call, in the same prefixed form.

3. **`grep` and `find` with no `path`** sweep both namespaces automatically.
   This needs no feature: it is what falls out of the default being unscoped.

4. **The `docs` tool's own description** carries the writable stores as a
   catalogue, the way `sql_query`'s description carries the connections:

   ```
   Write into a document store. Stores available:
     - product (PRDs, TRDs and decision records for the billing platform)
     - architecture (system design notes)
   ```

   Reads need no catalogue — `ls` is right there. Writes do, because the model
   has to pick a destination before it has looked at anything.

The dashboard gets a `DOCUMENT STORES` table next to the others, with the
document count and the resolved path, since that is a human's screen and a
human does want the real path.

### A naming collision, already settled

Three things want the word *docs*: `~/.drover/docs.md` (the reference),
`documents/` (the content), and the `docs` tool (writing PRDs). The directory
being `documents/` and the kind being `DocumentStore` keeps the disk
unambiguous. The MCP resource was named **`drover://reference`** rather than
`drover://docs` for the same reason, when it shipped — `drover://docs` would
read like it serves the PRDs, and it does not.

### The namespace: two of them, told apart by a prefix

A flat namespace shared by both kinds would mean a repository called `product`
and a document store called `product` can never coexist — one of them has to be
renamed, and neither of them is wrong. That is a bad trade for a rule nobody
asked for, so the kinds get **separate namespaces**, and the identifier that
says which one lives **in the path**:

```
api/src/main.go                bare  →  a repository
docs:product/prd-billing.md    docs: →  a document store
```

One rule: **a bare path is a repository; a `docs:` path is a document store.**
`product` and `docs:product` are then different things and both can exist.

The identifier belongs in the path rather than in a separate `source`
parameter, and the reason is round-tripping. A mixed `grep` returns hits from
both kinds; if the source were a second field, the model would have to carry
`(source, path)` as a pair from `grep` into `read` into `git blame` — and
models drop the second half of a pair. A path that says what it is survives
being copied anywhere:

```
grep "rate limit"
  api/internal/limit.go:44        →  read api/internal/limit.go
  docs:product/prd-billing.md:12  →  read docs:product/prd-billing.md
```

It also means **no new parameter on any tool.** The prefix does the work that a
`source` argument would have done, in the field that was already there:

| call | scope |
|---|---|
| `ls` | every root, repositories and stores, labelled |
| `ls docs:` | the document stores |
| `ls docs:product` | inside one store |
| `grep "rate limit"` | everything — the default stays a full sweep |
| `grep "rate limit"` `path: docs:` | documents only |
| `grep "rate limit"` `path: api` | one repository |
| `find "*.md"` `path: docs:` | documents only |

The unscoped sweep staying the default is the point of putting them in one
tool at all: *how is rate limiting meant to work, and how is it actually
implemented* is one `grep` across a spec and its implementation. The prefix
adds the ability to say "only the docs" — which a flat namespace could not
express either — without taking the sweep away.

Repositories stay bare rather than becoming `repo:`. Symmetry would be prettier,
but it would rewrite every path in the tests, the README, the lsp layer and
every existing agent transcript, to disambiguate a case that the `docs:` prefix
has already disambiguated. Asymmetry costs one sentence in a tool description.

### `git` and `lsp` are repositories only

Neither takes a prefix, because neither has anything to do in a store: there is
no history in a directory that is not a checkout, and no language server for a
tree of markdown. A `docs:` path handed to either is a clear error —

> `git` works on repositories; `docs:product` is a document store. Use `read`
> or `grep`.

— not a jail violation and not a missing file.

This also disposes of a trap that a flat namespace would have created. `git`
infers the repository when exactly one exists; with one namespace, applying a
first document store would silently make that inference ambiguous. With two,
stores are not candidates by construction and the inference cannot break.

### What changes in `internal/files`

`files.Root` is one directory today — `~/.drover/repos` — and the first path
segment is a repository name only by convention, because repositories happen to
be the subdirectories (`internal/files/files.go:46`).

It becomes a **root set**: named roots, each with a canonical directory, a kind
and a `writable` flag.

- A path is parsed into `(kind, root, rest)` before anything else. `docs:`
  selects the store namespace, its absence selects repositories.
- `resolve` then looks the root up *within that namespace* and jails inside its
  directory. Both existing checks stay, per root — lexical `..` rejection and
  `EvalSymlinks` re-containment.
- `List("")` returns every root from both namespaces, tagged, so `ls` can label
  them.
- `Grep`/`Find` with no `path` walk all roots; `docs:` alone scopes to the
  store namespace; a named root scopes to one tree.
- `display()` re-attaches the prefix, so what comes out is what can be passed
  back in.
- `Resolve` keeps its signature. The lsp layer was deliberately built to jail
  through this exact function rather than a second copy
  (`internal/files/files.go:118`), and since lsp only ever passes bare paths,
  it needs no change at all.

This refactor is the bulk of the work. It touches `internal/files`, the four
file tools, `internal/api`'s request types, and `internal/server`. Do it as its
own commit, read-only, with the existing tests still passing, before any write
tool exists.

### Traps in the refactor

- **A repository and a document store MAY share a name.** Two namespaces, so
  `product` and `docs:product` are different things. The existing rule stands
  unchanged — unique per kind — and pin *this* with a test too, because the
  obvious defensive patch is someone later adding a cross-kind uniqueness check
  that nothing needs.
- **`docs:` with no root is a scope, not a path.** `docs:` alone means "every
  store" for `grep`/`find`/`ls`, but it is not readable. `read docs:` must say
  so rather than falling through to a directory error.
- **A path is parsed before it is cleaned.** `filepath.Clean` on
  `docs:product/x` is fine, but the prefix has to come off first or the colon
  ends up in a path segment. Strip, then clean, then jail.
- **A colon can appear inside a filename.** Only a `docs:` prefix at position
  zero is an identifier; `api/weird:name.go` is a path. Match the prefix
  exactly, once, at the front — the same discipline the `{name}` placeholder
  scanner needed when it was reading `{"tenant":` as a parameter.
- **Lowercase-only applies to store names too.** The name becomes a path
  segment and macOS is case-insensitive.
- **An unknown root needs its own error, per namespace.** `foo/bar.md` should
  say *no repository named "foo"* and `docs:foo/bar.md` *no document store
  named "foo"* — each pointing at `ls`. Today anything not found under `repos/`
  is a missing file, which tells the caller nothing.
- **A store's directory is not a checkout and must never be reconciled.** The
  sync path resets to the remote with `checkout -B` + `reset --hard`. It
  refuses directories lacking `.git/drover-clone`, so a store is already safe
  by construction — but make it explicit, because a git-backed store (below)
  would otherwise walk straight into it.
- **A `path:` pointing inside `~/.drover/repos` must be rejected.** It would
  make a checkout writable through the side door and break the invariant that
  the write jail is a strict subset of the read jail.
- **Two stores may not nest.** Overlapping roots make `display()` ambiguous —
  one absolute path would map back to two different display paths.

### The write tool

One tool, an `operation` enum. That is the shape `git` and `lsp` already set,
and it is what keeps the tool count from growing with the feature:

**`docs`** — `write`, `edit`, `append`, `move`, `delete`.

| operation | args | notes |
|---|---|---|
| `write` | `path`, `content` | creates or replaces; makes parent directories |
| `edit` | `path`, `oldString`, `newString` | exact match, must be unique in the file |
| `append` | `path`, `content` | the common case for a running log |
| `move` | `path`, `to` | rename, within the same store |
| `delete` | `path` | one file |

Every `path` is `docs:`-prefixed, so a path found by `grep` pastes straight
into a `docs edit` with nothing to translate. A bare path — which by the one
rule means a repository — is refused here on its face, before any jail check
runs, with *`docs` writes to document stores; `api` is a repository*. That
makes the prefix a **second, independent guard** on the invariant that
checkouts are never written: the write tool would have to be handed a store
path *and* the root would have to be writable.

Reads stay on `read`, `ls`, `grep` and `find`. Nothing new for reading — which
was the requirement, and is also just correct: a second set of read tools over
the same paths would be two ways to do one thing.

`edit` taking `oldString`/`newString` rather than a diff is deliberate. Models
produce exact substrings reliably and unified diffs unreliably, and requiring
the match to be unique makes a wrong edit fail loudly instead of landing in the
wrong place.

### Decisions on writing

- **Advertise `docs` only when a writable store exists.** Same rule as
  `api_call` and `sql_query`: with nothing to act on there is nothing to
  advertise, and a tool that can only fail invites the model to try it.
- **The write jail is a strict subset of the read jail.** An agent can read
  every root and write only roots marked writable. Enforce in `internal/files`,
  not at the tool boundary — the same lesson as SQL read-only, which is
  enforced in the query path rather than only in the tool.
- **Use `atomicfile.Write`.** It already exists for exactly this reason
  (`internal/atomicfile/atomicfile.go`): a killed process must not leave half a
  file. A truncated PRD is worse than a missing one because nothing reports it.
- **Cap the size and refuse binary.** A document store is for text. Reuse
  `looksBinary` (`internal/files/files.go:362`) on the target before replacing
  it, so an agent cannot overwrite something that was never a document.
- **`delete` takes one file, never a directory.** Recursive delete through an
  agent is a whole class of incident for one line of convenience.
- **Frontmatter is the metadata model, and there is no index.**

  ```markdown
  ---
  type: prd
  status: draft
  owner: shekhar
  updated: 2026-08-21
  ---
  ```

  It is greppable — `grep "^status: draft"` is the query — and it survives
  being read by anything else. Building a metadata index over documents would
  be the vector-index mistake at small scale: a second source of truth that
  goes stale, in a project whose entire thesis is that grepping the real thing
  beats maintaining an index of it.

### The README sentence this breaks

> Everything an agent can reach is read-only. There is no tool that writes a
> file, no tool that POSTs, and no tool that writes to a database.

That is currently true and is part of the pitch. It has to be rewritten, and
precisely rather than quietly:

> An agent can never write to a checkout, never POST, and never write to a
> database. It can write only inside a document store you created for that
> purpose.

The invariant that actually matters — **the checkouts are untouchable** —
survives intact, because stores and repositories are different roots and the
writable flag lives on the root.

---

## Later: git-backed document stores

A store with a `url` where drover commits and pushes what the agent wrote. It
is the obvious next step and genuinely valuable — an agent's edit to a PRD
becomes a commit your team sees.

It is deliberately not in this plan, because it needs its own sync path.
Repository sync is a mirror: `reset --hard` to the remote, which would eat
every uncommitted document. A git-backed store needs pull-with-rebase, commit,
push, and a real answer for what happens on conflict. That is a plan of its
own, and it should not ride along with a refactor.

---

## Build order, if this is picked up

1. **`files.Root` becomes a root set, with the `docs:` prefix parsed.**
   Read-only, no new kind yet. Every existing test must pass **unchanged** —
   that is the proof the bare-path namespace was not disturbed. Add tests for
   the prefix parse, `docs:` as a bare scope, a colon inside a filename, an
   unknown root in each namespace, and nesting.
2. **The `DocumentStore` kind.** Apply, persist, delete, dashboard row,
   inventory line, and a test pinning that a repository and a store may share a
   name. Still read-only — a store you can `ls` and `grep` but not yet write.
3. **The `docs` tool.** Five operations, writable-root enforcement in
   `internal/files`, atomic writes, README rewrite.

The tool count goes from ten to eleven, once, and stays there.
