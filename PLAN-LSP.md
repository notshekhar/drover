# drover — git and lsp

Two tools that answer what `ls` / `read` / `grep` / `find` cannot.

`grep` sees the tree as it is now. **`git`** sees how it got that way.
**`lsp`** sees what it *means* — that this `Connect` is that `Connect`, and
these fourteen call sites are all of them.

Both are built and verified. What follows is what was decided and what it
cost; the open questions at the end are still open.

---

## Part 1 — `git` (built, 2026-08-20)

One MCP tool, ten operations behind an `operation` enum. The tool count goes
from eight to **nine**, not eighteen, for the same reason twenty HTTP requests
are one `api_call`: a tool list is the most expensive place to put a
catalogue.

| operation | answers |
|---|---|
| `log` | commits, newest first; filter by `path` `author` `since` `until` `grep` `merges` `limit`, or a `from`..`to` range. A single file's path gets `--follow`, so history survives renames. |
| `show` | one commit in full — message, parents, which files it touched, and with `patch=true` the diff itself. |
| `diff` | what changed between two revisions. `from` required, `to` defaults to HEAD. |
| `blame` | who last touched each line. `lines: "120-180"` windows it; defaults to the first 300. |
| `search` | git's pickaxe — the commits whose diff *added or removed* a string. This is how you find when something appeared or who deleted it. `regex=true` matches a pattern against the diff instead. |
| `file` | a file's contents **as of a revision**. The one thing `read` cannot do, since `read` only ever sees the checked-out tip. |
| `branches` | local and remote-tracking branches with their tips. |
| `tags` | tags, newest first, with the annotation message. |
| `contributors` | who commits and how often, narrowable by `path` / `since` / `until`, with each author's first and last commit here. |
| `status` | the checkout itself: url, branch, HEAD, commit count, root-commit date, last fetch — and whether the tree is dirty. |

### What it does not do, on purpose

Read-only by construction. Every operation maps to a git command that only
inspects — `log`, `show`, `diff`, `blame`, `for-each-ref`, `rev-list`,
`status`. Fetching stays the reconciler's job. Nothing here can write to a
checkout, and nothing here reaches the network.

### Decisions worth remembering

- **The repository is inferred when there is exactly one**, and demanded (with
  the list, in the error) when there are several. The common case costs no
  round trip; the ambiguous case does not get guessed at.
- **Both path forms work.** The file tools hand out repository-prefixed paths
  (`api/internal/db.go`), so a model that just grepped will pass one. One
  `TrimPrefix` turns a guaranteed failed call into a working one.
- **`--shortstat` needs its own field.** The commit format ends with a field
  separator so the trailing stat line lands in a field of its own instead of
  being glued to the body. \x1e / \x1f as record and unit separators, because
  a commit body contains newlines and a line-oriented format cannot be parsed
  back.
- **`--name-status` and `--numstat` are mutually exclusive**, and `--numstat`
  alone cannot tell an addition from a modification. So the diff runs twice
  and the two `-z` outputs are zipped: `--name-status` is what says which
  records carry two paths because they are renames.
- **A merge shows an empty diff by default.** `show` adds `-m --first-parent`
  for a merge commit, or it reports that a merge touched nothing.
- **A dirty checkout is reported as a warning, not a fact.** drover resets
  these trees on every sync, so anything uncommitted in one is about to be
  destroyed.
- **Argument hardening**: `rev`/`from`/`to` may not start with `-` or contain
  newlines, a repository name is one path segment, `..` in a path is refused,
  and stdout is capped at 16MB per process so a patch against a vendored tree
  cannot be held in memory before being thrown away.

### Verified

`go test ./internal/git/` — 11 tests over a fixture with two authors, a
rename, a deletion and an annotated tag, plus nine refusal cases. Live against
drover's own checkout through `POST /apis/drover/v1/git` and through the MCP
`tools/call` endpoint: `log`, `blame`, `search`, `contributors`, `status` all
render correctly.

### Left open

- No `drover git <op>` CLI subcommand. The REST endpoint and the MCP tool
  exist; a CLI would only be for debugging without an agent.
- **Single-branch clones.** `repo.Reconcile` clones with `--single-branch`, so
  `branches` will almost always show one and `log` cannot reach another
  branch's history. Cross-branch questions would need a `spec.fetchAll` on
  Repository. The tool says so in its own description rather than looking
  broken.

---

## Part 2 — `lsp` (built, 2026-08-20)

TypeScript 7, Go, Java. One more tool, ten more operations behind an enum:
**ten tools, fixed.**

### Why this is a different problem in drover than in loop

loop's LSP is a builtin extension in a CLI: one workspace, per-`cwd` manager,
servers spawned on demand and reaped on `deactivate()`, diagnostics pushed
back through `tools.onResult(["write","edit"])`.

drover is a **daemon holding N checkouts and serving many agents**. Four
consequences, and they are the whole design:

1. **The cold start becomes free.** gopls on a large repo takes tens of
   seconds to index; jdtls takes minutes. In a CLI every session pays that. In
   drover it is paid once, by the daemon, and every agent that connects
   afterwards asks a warm server. This is the strongest argument for the
   feature existing at all.
2. **There are no edits, so there is no diagnostics-on-write.** drover has no
   write tool to hook. Diagnostics become an explicit operation — "what does
   the compiler say about this file" — rather than the centrepiece they are in
   loop.
3. **`git reset --hard` runs under the server's feet.** Reconcile rewrites the
   working tree on every sync. This is the drover-specific trap, and it has no
   analogue in loop. See below.
4. **There is no Node and no Bun.** drover is one static Go binary. loop
   provisions servers with `bun install`; drover cannot, and must not start
   requiring a JS runtime to read a `.ts` file.

### The three servers

**TypeScript 7 — `tsgo` / `tsc --lsp --stdio`**

TypeScript 7 is a native Go binary that speaks LSP itself. No
`typescript-language-server`, no Node in front of it. The npm package is a
thin shim over `@typescript/typescript-{platform}-{arch}/lib/tsc`.

Acquisition without npm: the registry is just HTTPS. `GET
registry.npmjs.org/@typescript/typescript-darwin-arm64` → JSON →
`versions[v].dist.tarball` → a `.tgz` that Go's `archive/tar` +
`compress/gzip` unpack from the stdlib. **Zero dependencies, no Node on the
box.** Strictly cleaner than loop's route here.

Deliberately **not** reusing a checkout's own `node_modules/.bin/tsc`. loop
had to add `minMajorVersion: 7` because nearly every TS project pins a
TypeScript 5 that fails the `--lsp` handshake and takes the whole language
down with it (v0.15.5 was broken for almost everyone). drover's checkouts are
mirrors — nobody ran `npm install` in them, so `node_modules` is not there in
the first place. Downloading our own pinned 7 makes that entire class of bug
unreachable.

**Go — `gopls`**

PATH first, then `go install golang.org/x/tools/gopls@latest` into
`~/.drover/servers/bin`. There is no prebuilt release to download, so a box
with no Go toolchain gets a clear refusal rather than a silent absence. Root
markers `go.mod` / `go.work`.

**Java — `jdtls`**

Eclipse's rolling snapshot tarball, run under the user's own JVM 21+. Every
trap loop measured on this route carries over unchanged and is worth copying
rather than rediscovering:

- **A total-duration download timeout is the wrong shape.** Eclipse serves the
  28MB snapshot at ~120KB/s — four minutes of *healthy* transfer. Use a stall
  timeout reset per chunk, not a cap on the whole thing.
- **`config_mac_arm` really exists.** Probe for it and fall back to
  `config_mac`; hardcoding the x86 config is what opencode does and Apple
  Silicon pays for it.
- **JVM flags must precede `-jar`.** After it they are application arguments,
  so `-Declipse.application` never gets set and the launcher does nothing.
- **Each `--add-opens` is one argv element.** `"--add-opens a=b"` as a single
  string is a flag literally named `--add-opens a=b`.
- **macOS `/usr/bin/java` is a stub** that exists on PATH and reports no
  runtime. Check `java -version` and fail closed before a 50MB download.
- jdtls needs a writable `-data` workspace. It goes in
  `~/.drover/servers/java/workspaces/<repo>-<hash>` — outside the checkout, so
  the "nothing writes to a checkout" guarantee still holds exactly.

### The operations

Same shape as `git`: one tool, an `operation` enum, arguments shared.

| operation | answers |
|---|---|
| `definition` | where is this defined |
| `references` | every place it is used |
| `hover` | its type and doc comment |
| `implementations` | who implements this interface |
| `document_symbols` | a file's outline |
| `workspace_symbols` | find a symbol by name across the repo — "grep, but it knows what a symbol is". Likely the most-used operation. |
| `incoming_calls` | who calls this |
| `outgoing_calls` | what this calls |
| `diagnostics` | what the compiler says about this file |
| `servers` | what is running, for which repository, and **why a language is unavailable**. loop has no equivalent, and a server that silently failed to start is otherwise indistinguishable from a language nobody asked about. |

### Two argument decisions

**Paths are repository-prefixed**, exactly as the file tools return them
(`api/internal/db.go`). `grep` → `lsp` must compose with no translation step.

**`symbol` instead of `character`.** An agent gets `file:line` from grep and
knows the *identifier text*; it does not know the column. Demanding
`character` is the single largest source of failed LSP calls. So: give
`symbol: "Connect"` and drover finds the column on that line itself
(`occurrence: 2` for the second one). Omit `line` entirely and it falls back
to `workspace_symbols`. Coordinates the model does supply are 1-based, matching
what `read` and `grep` print; LSP is 0-based; the conversion happens once, at
both ends.

### Package layout

```
internal/lsp/
  protocol.go   JSON-RPC over Content-Length framing; Position/Range/Location/
                DocumentSymbol/Diagnostic/Hover; path <-> file:// URI
  client.go     one process; a reader goroutine; map[id]chan result;
                Initialize / Request / Notify / OpenDocument / Diagnose / Shutdown
  registry.go   the three server definitions, declarative
  acquire.go    PATH -> go install -> npm tarball -> release download,
                all into ~/.drover/servers/
  manager.go    keyed <serverKey>@<repo>/<root>; lazy start, singleflight,
                idle reap, restart-on-sync
  ops.go        the ten operations and how each renders
```

Wired exactly like `git`, so the two cannot drift: `api.LSPRequest` /
`LSPResponse`, `POST /apis/drover/v1/lsp`, `client.LSP()`, `mcp.Backend.LSP()`,
tool number ten.

### The four traps to design for up front

1. **Sync invalidation — the drover-specific one.** `Reconcile` does `git
   reset --hard`, rewriting files a running server has already parsed. A server
   that indexed the old tree will answer confidently and wrongly. Sending
   `workspace/didChangeWatchedFiles` is the polite fix and is unreliable across
   servers. **Restart the servers for that repository whenever a sync changes
   the commit.** It is cheap and it is correct, and the daemon is what makes it
   affordable.
2. **Pull vs push diagnostics.** The old model volunteers
   `textDocument/publishDiagnostics`; the modern one (`diagnosticProvider`,
   which TS7 uses) answers only when asked via `textDocument/diagnostic`. loop
   only did push and TS7 reported every file clean — the feature silently did
   nothing while looking like it worked. Ask when the capability is advertised,
   merge with anything pushed, and never sit waiting for a push from a
   pull-only server.
3. **Indexing is not an error.** jdtls on a large Java repo indexes for
   minutes. Track `$/progress` and answer "still indexing, ask again" rather
   than timing out into something that reads like a failure.
4. **Dependencies are not installed.** A drover checkout never had `npm
   install`, `go mod download` or `mvn` run in it. Intra-repo navigation works;
   `import x from "react"` will not resolve. This is a real limitation and
   belongs in the tool description rather than in a bug report. Fixing it means
   executing the repository's own build tooling — postinstall scripts and all —
   which is a boundary drover currently does not cross. **Open decision.**

### What got built

```
internal/lsp/
  protocol.go   LSP types, path <-> URI, and the shape normalisers
  client.go     one process, one reader goroutine, id-keyed pending map
  registry.go   the three definitions, declarative
  acquire.go    PATH -> go install -> npm tarball -> release download
  manager.go    lazy start, singleflight, idle reap, LRU cap, restart-on-sync
  ops.go        the ten operations
```

Wired exactly like `git`: `api.LSPRequest`/`LSPResponse`, `POST
/apis/drover/v1/lsp`, `client.LSP()`, `mcp.Backend.LSP()`, tool ten.
`Options.ServersDir` and `Options.NoServerInstall` exist so tests never touch
the network, and `DROVER_NO_SERVER_INSTALL=1` is the same switch for a user.

Phases 0 through 3 all landed, except the `drover dash` table and the
`LanguageServer` object kind, which are still open.

### Measured on the way through

- **gopls handshakes in 30–150ms** against a real module, and answers
  definition, references, hover, document/workspace symbols and both call
  directions correctly.
- **TypeScript 7 installs from the npm registry in 2.9s** — 9MB, straight
  HTTPS, `archive/tar` + `compress/gzip`, no npm and no Node anywhere. The
  extracted `package/lib/tsc` is a Mach-O arm64 binary reporting 7.0.2, and it
  handshakes.
- **Pull vs push is real and split down the middle.** TS7 advertises
  `diagnosticProvider: true` and answers only when asked; gopls advertises
  `false` and volunteers. A client that implements one gets silence from the
  other, so `Diagnostics` does both and merges.
- **gopls ignores `linkSupport`.** It was asked for `LocationLink` and sent
  `Location`. Decoding only the requested shape yields an empty array that
  reads as "no definition found" — so `ToLocations` accepts all four shapes
  (single or array, either type).
- **A server request that goes unanswered is a hang.** gopls sends
  `workspace/configuration` during initialization and waits. The client answers
  every server-initiated request, with one config entry per item requested.
- **The macOS `java` stub declines in 13ms**, before any download, with a
  message naming the toolchain and the version needed.

### Two decisions worth keeping

**`symbol` instead of `character`.** An agent arrives from `grep` holding a
name, never a column. `{"operation": "references", "path": "…", "symbol":
"Connect"}` works; the column is worked out here, whole-word, skipping comment
lines. When the name appears on several lines the answer says so and names
them, rather than guessing silently.

**Paths are repository-prefixed**, exactly as `ls`/`grep`/`read` return them,
and results come back the same way — so an `lsp` result can be handed straight
to `read` or to `git blame` with no translation. Resolution goes through
`files.Root.Resolve`, the same jail the file tools use, rather than a second
copy of it.

### Still open — these are yours

Decision 1 was taken, then reversed on user correction, and the correction was
right. It was built off-by-default behind `--lsp` on a memory argument. But
`api_call` and `sql_query` are conditional because without a configured request
or connection there is nothing for them to act on; `lsp` acts on the same
checkouts `grep` does, so if there is anything to grep there is something to
navigate. And the memory argument was already answered by the lifecycle: lazy
start, idle reap, LRU cap. A server nobody asks about is never launched, so the
flag was protecting against a cost that only exists once you are already using
the thing. **`lsp` is now unconditional, like the file tools.** What survives is
a switch on the network rather than on the feature:
`DROVER_NO_SERVER_INSTALL=1`.

The rest are untouched:

2. **May a language server reach the network?** gopls will want to fetch
   modules it does not have cached. That is a network *read*, not a write, so
   it does not break the read-only guarantee — but it does break "drover only
   talks to the remotes you gave it". Suggest `GOFLAGS`/`GOPROXY` under a
   per-Repository `offline: true`.
3. **Do we ever run a repository's own tooling** to make cross-package
   navigation work (trap 4)? Recommendation: no, not by default, and never
   without an explicit per-Repository opt-in that names postinstall scripts as
   the reason it is opt-in.
4. **A fourth kind, `LanguageServer`?** Very on-brand — a fourth language
   becomes `drover apply -f java.yaml` instead of a release. Not needed for
   three, so phase 3.

### Known gaps

- **Java is unverified.** The registry entry, the JVM gate, the config_mac_arm
  probe and the jvmArgs ordering are all written and the gate is tested, but
  this machine has no JVM, so jdtls has never actually been downloaded or
  launched here. Everything past the gate is code review, not evidence.
- **No per-Repository control.** Language servers are engine-wide.
- **No `drover dash` table** for running servers. The `servers` operation is
  the only place to see them.
- **No `LanguageServer` object kind**, so a fourth language is still a release.
- **Dependencies are still not installed** in a checkout (trap 4). Unchanged
  and, per the recommendation below, deliberately so.

### The reason to build it

`grep` finds the string. `lsp` finds the *symbol* — and then `git blame` on
that line, and `git show` on the commit blame names, tells you why it is like
that. Three tools that compose into the question an agent actually has, which
is never "where does this string appear".
