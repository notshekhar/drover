# drover — plan: logs, traces, metrics and cluster state

**Status: not built.** Nothing here has run. The CubeAPM endpoints in §4 were
read off a third-party client's source and the vendor docs, not called from
this machine; everything labelled a *trap* is predicted from how the format or
the protocol works, not measured. That distinction is kept throughout, and
phase 1 exists mostly to convert it.

`git` says how the code got that way. `lsp` says what it means. Neither says
what it **did** last Tuesday at 03:14. That is the gap.

---

## 1. The shape of the problem here

Telemetry is split three ways, and the split is the whole difficulty:

| tier | source | holds | window |
|---|---|---|---|
| **now** | `kubectl logs` | everything, unshipped | minutes |
| **hot** | CubeAPM | **errors only** | retention, ~30d |
| **archive** | S3 | everything else | forever, cold |

An agent investigating an incident needs all three and can reach none of them.
Worse, the middle tier lies by omission: query CubeAPM for the request that
failed and you get the error line and nothing around it, because nothing around
it was ever sent there. An empty result reads as "that did not happen" when it
means "that tier does not carry this".

So the first design rule: **a tier declares what it covers, in prose, and that
prose is handed to the agent.** Not a schema, not a level enum — a sentence,
because the honest answer ("ERROR and FATAL only, everything below goes to S3")
is a sentence.

The second: **the error is in CubeAPM and the reason is in S3**, in the two
hundred lines around it that were never shipped hot. A tool that reaches one
tier is a worse version of the UI you already have. The feature is the tool
that spans them.

## 2. The move: every result is a file

This is what makes it drover and not another observability MCP server.

**Every log line drover fetches, from any tier, is normalised and written into
the tree**, under a second file root next to `repos/`. `ls`, `read`, `grep` and
`find` already reach it, jailed the same way, with no new search semantics.

Three things fall out, and they are the argument for the whole design:

1. **Re-searching is free and exact.** The first call costs a CubeAPM round
   trip or an S3 GET. Every grep after it is local. An investigation greps the
   same corpus five or six times; today that is five or six vendor queries.
2. **The tiers merge on disk.** Hydrate the archive window, drop the hot tier's
   hits into the same directory, sort by event time, and one `grep` sees the
   error *and* the non-error lines around it. Cross-tier correlation is not a
   feature that gets built — it is what happens when both tiers write files
   into one directory.
3. **Code and logs are one search.** `grep` over a hydrated window and over the
   service that emitted it is one call, one jail, one result format.

The cost is disk, and disk is the cheapest thing in this system. It is capped
(§6) and evictable.

## 3. `kind: LogSource`

```yaml
apiVersion: drover/v1
kind: LogSource
metadata:
  name: prod
spec:
  tiers:
    - name: hot
      provider: cubeapm
      url: http://cubeapm.internal:3140
      covers: "ERROR and FATAL only; everything below goes to the archive tier"
      retention: 30d
      defaultStream: '{env="prod"}'      # prepended when the query has no selector

    - name: archive
      provider: s3
      bucket: acme-logs
      region: ap-south-1
      layout: "{service}/dt={date}/{hour}/"   # or omit -- inferred, see 5.1
      covers: "every log line, including the ones the hot tier drops"
      timeField: auto                     # auto | <field name>
      traceField: auto                    # auto | <field name> | none
      serviceField: auto
      padding: 15m                        # over-fetch either side; see trap 5
      budget: 512MiB                      # refused above this, per call
      cache: 5GiB                         # LRU across all windows of this source
      manifest: 6h                        # key-listing refresh, like refreshInterval
      redact: []                          # regexes scrubbed before anything is written
```

Rules, in the house style:

- **Tiers are ordered, and the order is the truth about freshness.** `now`
  before `hot` before `archive`. A query with no explicit tier walks them in
  order and reports which one answered.
- **`covers` is required.** A tier that will not say what it holds cannot be
  advertised, for the same reason a `SQLConnection` with no `health` gets no
  tool. Silence about coverage is the bug in §1.
- **Credentials are never inline.** S3 credentials resolve exactly like
  `SQLConnection` urls: `${ENV}` references, `~/.aws/credentials` by profile,
  or `aws configure export-credentials` shelled out for SSO and assume-role.
  An inline `secretAccessKey` is refused at apply. Same rule as everywhere
  else: a credential cannot be committed by accident.
- **`auto` fields are resolved by measurement, not by guessing** — §5.2.

## 4. The hot tier: CubeAPM

CubeAPM is three well-known wire protocols behind one host. There is no vendor
SDK to take, which keeps the zero-dep, no-cgo, one-static-binary story intact —
`net/http` and `encoding/json` cover all of it.

Query APIs are on port **3140** (ingest is 3130, and drover never ingests):

| signal | endpoint | protocol |
|---|---|---|
| logs | `POST /api/logs/select/logsql/query` | VictoriaLogs **LogsQL**, form-encoded `query`, `limit`; NDJSON response |
| metrics | `GET /api/metrics/api/v1/query`, `/query_range` | Prometheus / VictoriaMetrics PromQL |
| traces | `GET /api/traces/api/v1/search`, `/api/traces/api/v1/traces/{id}` | Jaeger |

**Unverified.** Those paths come from the vendor's docs plus the source of
`TechnicalRhino/cubeapm-mcp`, and no auth is documented because the query port
is expected to be private. Phase 1 calls all five against the real engine
before any of it is advertised, and `spec.tiers[].headers` exists for the case
where it turns out there is auth after all.

### Four traps, all predicted

1. **The time range is not a parameter.** LogsQL carries it *inside* the query,
   as a `_time:[start, end]` filter. Send `start=` and `end=` as form fields and
   they are ignored — you get the default window and no error. drover takes
   `from`/`to` as real arguments and injects the filter, and refuses a query
   that already contains its own `_time:` rather than producing two.
2. **A query with no stream selector scans everything.** `{service="api"}` in
   front is the difference between a second and a timeout. `defaultStream` is
   prepended when the model's query has no `{...}`, and a wide window with no
   selector is refused with the reason.
3. **Histograms are `vmrange`, not `le`.** A model that has seen Prometheus
   will write `histogram_quantile(0.95, ...le...)` and get an empty result with
   a 200 status — the worst failure shape there is. The `metrics` tool
   description states the convention (`histogram_quantiles("phi", 0.95, sum by
   (vmrange) (...))`, `cube_apm_*` prefix, `service` not `service_name`,
   latency in seconds) because that is cheaper than the model discovering it.
4. **The response is NDJSON, not JSON.** Decode line by line with a byte cap as
   well as a line cap; a `stats` pipe returns a different shape from a plain
   search, so both are handled and neither is assumed.

## 5. The archive tier: S3

No Athena, so this is hydrate-and-grep: list, fetch, decompress, normalise,
write, then let the file tools do what they already do.

S3 without `aws-sdk-go-v2`. The SDK would roughly double a 4.5 MB binary;
SigV4 plus `ListObjectsV2` plus `GetObject` is about 250 lines of stdlib, and
the ugly part of the SDK — credential resolution — is delegated to the `aws`
CLI the same way `git` and `go install` are already shelled out to. If the CLI
is absent, env vars and `~/.aws/credentials` still work; SSO does not, and says
so.

### 5.1 The manifest

A listing is not free and a wide `ListObjectsV2` is thousands of round trips,
so the key listing is **an object like any other, reconciled on a schedule** —
`manifest: 6h` is `refreshInterval` under another name.

```
~/.drover/logs/prod/manifest/
  keys-2026-08-25.tsv     key \t size \t lastModified, one per line
  layout.yaml             the inferred or declared partition template
  schema.md               what the lines actually look like -- 5.2
```

TSV on disk, so "do we even have that day" is a `grep`, and so a manifest that
is stale in an interesting way is visible rather than inferred.

When `layout` is omitted it is **inferred from a listing**: Hive-style
partitions (`dt=`, `year=`, `hour=`, `service=`) are recognised, and so are
bare date-ish path segments. The inference is written to `layout.yaml` and
reported by `drover get logsource prod` — inferred, shown, overridable. Never
inferred silently.

### 5.2 Sniffing, because "not sure" is the honest answer

An OTel collector, Vector and Fluent Bit all write S3 and none of them agree on
what they write. The extension lies (Vector's `.log.gz` may be NDJSON; the OTel
`awss3exporter` may be OTLP-JSON), so drover reads bytes.

On first sync of the tier, and on every layout change, drover pulls **one
object**, decompresses it, reads the first N records, and writes what it found:

- **Container**, by magic bytes: gzip (`1f 8b`), zstd (`28 b5 2f fd`), plain.
- **Record framing**: newline-delimited JSON, a JSON *array* per object
  (Fluent Bit does this), or unstructured text.
- **Envelope**: a record with `resourceLogs[]` is OTLP-JSON and is flattened;
  anything else is taken as one flat object per record.
- **Fields**, by candidate match and then by hit rate over the sample:
  time (`timestamp`, `ts`, `time`, `@timestamp`, `observedTimeUnixNano`),
  trace (`trace_id`, `traceId`, `trace.id`, `request_id`, `x-request-id`,
  `correlation_id`), service, severity.

The result is `schema.md`, in prose, and it is what `drover get logsource` and
the MCP inventory report:

```
archive tier, sampled 500 records from acme-logs/api/dt=2026-08-24/11/
  container   gzip (concatenated members)
  framing     newline-delimited JSON
  envelope    OTLP-JSON  ->  flattened, resource attributes hoisted
  time        observedTimeUnixNano   100%
  service     resource.service.name  100%
  severity    severityText            98%
  trace       traceId                 71%   <- absent on 29% of sampled records
```

**This is the answer to not knowing whether the lines carry a trace id.**
drover measures it, states the hit rate, and the `trace` operation on this tier
either works or explains precisely why it cannot — rather than returning an
empty result that reads like "no such trace".

If no trace field is found, correlation degrades to service plus time window,
which is still "the two hundred lines around this error", and the tool says
that is what it did.

### 5.3 Normalisation, and why it is the point

OTLP-JSON is nested three levels deep with attributes as arrays of
`{key, value: {stringValue}}` objects. Grep over that returns a 4 KB line in
which the match is invisible. **Grep over unnormalised telemetry is not exact,
it is unreadable**, and unreadable defeats the entire thesis.

So a hydrated window is written as one line per record, stable field order,
event-time sorted:

```
2026-08-25T10:04:03.221Z ERROR api trace=4bf92f... msg="dial tcp: connection refused" http.route=/v1/users upstream=billing
```

Greppable *and* readable, which NDJSON is not and which is worth more than
structure here. The original bytes are kept in `raw/` under the same window, so
nothing is lost and a re-normalisation never re-fetches — **except** when
`spec.redact` is set, in which case `raw/` is not kept at all, because a
redaction with the unredacted copy beside it is not a redaction.

### 5.4 Cost, which is the real hazard

A careless window is tens of gigabytes of GETs and egress, and a model will try
the wide query first every single time.

- **`budget` is a refusal, not a truncation.** Over it, the call fails with the
  count and the size and the advice to narrow — a partial answer from a
  truncated window is a wrong answer about logs.
- **Estimating is a first-class operation.** `logs operation=estimate` answers
  from the manifest alone, with no GETs: *"12,400 objects, 38 GiB, 6 services
  — narrow the window or name a service."*
- **A hydrated window is never re-fetched.** `cache` is an LRU over total
  bytes; the meta file records what was fetched so a superset window reuses
  what it can.

## 6. `kind: Cluster`

```yaml
apiVersion: drover/v1
kind: Cluster
metadata: {name: prod}
spec:
  kubeconfig: ${KUBECONFIG}
  context: prod-ap-south-1
  namespaces: [billing, api]     # allowlist; empty means every namespace
  timeout: 20s
  health: canIWrite              # the gate -- see below. no opt-out.
```

**It shells out to `kubectl`**, for the reason `git` shells out to `git`:
`client-go` is tens of megabytes of dependency, and kubectl already implements
kubeconfig, contexts, and — the part that actually matters — exec credential
plugins, so EKS/GKE/SSO auth keeps working with no code here.

### 6.1 No writes. Four layers, and the last one is the only one that counts.

Everything an agent can reach in drover is read-only, and a cluster is the
place where that guarantee is hardest to keep and most expensive to lose. A
verb allowlist alone does **not** hold — `kubectl` has too many ways to turn a
read into something else. So, in order of increasing trust:

**Layer 1 — argv is constructed, never composed.** The tool takes structured
parameters (`resource`, `name`, `namespace`, `selector`, `output`, `since`,
`container`), and drover builds the argument vector itself. There is no field
anywhere in the tool schema whose value lands on the command line as a flag,
and nothing is ever handed to a shell — `exec.Command`, never `sh -c`. This is
the layer that closes flag smuggling, and flag smuggling is the real attack:
`--as=system:admin` is privilege escalation, `--kubeconfig` and `--server`
retarget the whole call, `--v=9` prints the bearer token into the output, and
`-w` turns a read into a process that never exits. None of those are reachable
because there is nowhere to put them.

**Layer 2 — the verb is one of six.** `get`, `describe`, `events`, `logs`,
`top`, `api-resources`. An allowlist, not a denylist, checked on the verb
drover itself chose. Notably absent and never reachable: `exec`, `attach`,
`port-forward`, `cp`, `proxy`, `edit`, `patch`, `apply`, `replace`, `scale`,
`rollout`, `delete`, `drain`, `cordon`, `debug`. Several of those are not even
writes — `exec` and `port-forward` are *pivots*, which is worse: they turn a
context engine into a way into the network.

Two more things this layer refuses, which are neither writes nor obvious:
`kubectl <anything-else>` resolves to a `kubectl-<anything-else>` plugin binary
on `PATH`, so an unknown verb is arbitrary code execution, not an error; and
`get --raw` takes an arbitrary API path, which stays a GET but leaves the
allowlist's model of what is being read.

**Layer 3 — resource types are filtered.** `Secret` is refused as a type
rather than redacted, the same shape as the SQL write gate: fail closed on the
kind, do not try to sanitise the output. And because a `DATABASE_URL` in a pod
env var is not a Secret object, the ledger's redaction (PLAN-WEB §4) runs over
every response before either the agent or the ledger sees it.

**Layer 4 — the credential cannot write, and drover proves it.** The three
layers above are drover's own code, and drover's own code can have a bug. The
only guarantee that survives one is a credential that is incapable of the thing
in the first place.

So `health` on a Cluster is a **gate, exactly like `health` on a
SQLConnection** — and it asks the cluster about drover itself:

```
kubectl auth can-i create pods
kubectl auth can-i delete pods
kubectl auth can-i create secrets
kubectl auth can-i '*' '*'
```

Any of them answering `yes` and **no `cluster` tool is advertised at all**.
The dashboard shows the cluster as stored-but-not-offered — the same `○` a
non-GET HTTPRequest gets — with the reason: *"these credentials can create
pods; bind a read-only ServiceAccount."* There is no `readOnly: false` escape
hatch on this kind, because unlike a database there is no legitimate reason for
an agent to write to a cluster.

The README says what to bind:

```yaml
kind: ClusterRole
rules:
  - apiGroups: ["", "apps", "batch", "networking.k8s.io"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]      # and nothing else, ever
```

`view` is close but grants `get` on ConfigMaps across the cluster and is a
moving target between versions; a bound role written down here is checkable.

The gate re-runs on the same schedule a SQLConnection's does, so a credential
that gets broadened later stops being offered rather than quietly becoming
dangerous.

### 6.2 The now tier

Pod logs land on disk exactly like the other two tiers, so one grep spans "the
last four minutes" and "last Tuesday".

Trap, predicted: an exec credential plugin that wants to open a browser or
prompt will **hang**, not fail. Every kubectl invocation carries a context
deadline, and a timeout is reported as "the credential plugin did not return —
run `kubectl get pods` yourself once to prime it", because that is the actual
fix.

Trap: `--previous` is what you want for a crashlooping pod, and a model will
not think of it. `cluster logs` takes `previous: true` and the description says
when to reach for it.

Trap: `top` needs metrics-server, which plenty of clusters do not run. Absent,
it is reported as "metrics-server is not installed", not as an error the model
will retry.

## 7. Tools

Four new, none of which grows with the number of objects. Ten fixed becomes
**fourteen fixed**.

### `logs`

| operation | does |
|---|---|
| `sources` | the sources, their tiers, each tier's `covers` sentence, and what the manifest knows |
| `estimate` | manifest-only: how much a window would cost, no fetching |
| `search` | search a tier. **Hot: LogsQL. Archive: RE2**, because the archive is a file tree and RE2 is what `grep` already speaks |
| `around` | N lines either side of a timestamp, spanning tiers |
| `trace` | every line for one trace id across every tier that can answer |
| `tail` | the now tier: pod logs, bounded |
| `hydrate` | materialise a window and return the path, for iterative grepping |

`search` on the archive hydrates and greps in one call and **returns the path
it wrote**, so the follow-ups are free local greps. `hydrate` exists because
the model that intends to ask six questions should be told to hydrate once.

The two query languages are a real seam and it is deliberate: translating
LogsQL to RE2 would be lossy in one direction and impossible in the other. The
tool description states which tier speaks which, and a LogsQL pipe sent to the
archive is a clear error, not an empty result.

### `trace`, `metrics`, `cluster`

`trace`: `search`, `get`, `services` against the Jaeger API. `get` reports the
archive window that would cover the trace's span and offers to hydrate it —
**that is the killer path**: error in the hot tier, trace id from it, whole
request timeline out of the archive, and the code that emitted each line is in
the same jail.

`metrics`: `instant`, `range`, `series`, `labels`, PromQL, with the CubeAPM
conventions in the description (trap 3).

`cluster`: `get`, `describe`, `events`, `logs`, `top`, `api-resources`.

If fourteen is one too many, `metrics` and `trace` collapse into one `apm` tool
behind an `operation` enum — they are one endpoint family. `logs` and `cluster`
do not collapse into anything; they are different questions.

## 8. Storage, and the second file root

```
~/.drover/
  logs/<source>/
    manifest/                     keys-*.tsv, layout.yaml, schema.md
    windows/<from>_<to>_<service>/
      lines.log                   normalised, event-time sorted, greppable
      raw/                        original objects (absent when redact is set)
      meta.yaml                   tier, objects, bytes, fetched-at, coverage
```

**Phase 0 is a refactor, and everything else waits on it.** The file jail is
`$DROVER_DATA/repos` today, single-rooted. It becomes **multi-rooted** —
`repos/`, `logs/`, and (from PLAN-WAREHOUSE) `docs/` — with `ls` on no path
listing the roots, resolution still going through `files.Root.Resolve`, and
`..` and symlink escapes rejected exactly as now. One jail, several roots, not
several jails.

## 9. Wire API and CLI

Same seam as `git` and `lsp`: `api.LogsRequest`/`LogsResponse`, `POST
/apis/drover/v1/logs`, `client.Logs()`, `mcp.Backend.Logs()`. One execution
path that both the CLI and MCP drive, per PLAN-WEB §2 — never two.

```
drover logs <source> "<query>" [--tier hot|archive|now] [--from -1h] [--to now]
drover logs estimate <source> --from -24h --service api
drover logs hydrate <source> --from -6h --service api
drover trace <source> <trace-id>
drover metrics <source> "<promql>" [--range 1h --step 60]
drover cluster get pods -n billing
drover schema <logsource>            re-sniff the archive tier and rewrite schema.md
```

Every one of them lands in the activity ledger with attribution, duration, the
tier that answered, and bytes fetched. Bytes fetched is a cost line and belongs
on the dashboard.

## 10. Build order

| phase | what | done when |
|---|---|---|
| 0 | multi-root file jail | `ls` lists `repos` and `logs`; every existing jail test still passes |
| 1 | `LogSource`, hot tier, `logs search`/`tail`, results written to disk | all five CubeAPM endpoints called live from this machine; §4's four traps confirmed or corrected |
| 2 | archive tier: SigV4, listing, manifest, sniffing, `schema.md`, hydrate, budget, estimate | a real window from the real bucket hydrated and grepped; `schema.md` matches what the collector actually writes |
| 3 | `trace` + `metrics` | a trace id from a hot-tier error resolves to a full timeline |
| 4 | `Cluster`, kubectl shell-out, now tier | all four read-only layers tested: no flag reaches argv, an unknown verb never resolves to a plugin, `Secret` refused, and a writable credential leaves the tool unadvertised |
| 5 | correlation: `logs trace` spanning tiers, `trace get` offering the window | the §1 sentence is one tool call |
| 6 | TUI and web dashboard rows: sources, tiers, cache size, bytes fetched | — |

## 11. Traps, all predicted, none measured

- **Concatenated gzip members.** Firehose-style writers append gzip streams
  into one object. `compress/gzip` reads the first member and stops unless the
  reader is looped with multistream — a naive read silently truncates the
  object, which looks like a quiet hour rather than a bug.
- **Objects are named by flush time, not event time.** An event at 10:59 lands
  in the 11:00 object routinely, and under backpressure a much later one. A
  window fetched exactly is a window with holes at both ends. Hence `padding`,
  defaulting to 15m each side, and it must be documented, because a model
  asking for exactly one minute deserves to be told it got three.
- **Fluent Bit writes a JSON array per object**, not NDJSON, and can be
  configured to write msgpack. Sniff; never trust the extension.
- **OTLP-JSON attribute arrays** must be hoisted to flat keys or grep is
  worthless (§5.3).
- **SigV4 details**: sorted canonical query, `~` unescaped, `UNSIGNED-PAYLOAD`
  for GET, and the clock skew window — a machine 6 minutes off gets 403s that
  read like a permissions problem.
- **`ListObjectsV2` pages at 1000 keys.** A day of hourly objects across 40
  services pages many times; the manifest exists so this happens on a schedule
  and not inside a tool call.
- **LogsQL's `_time` filter** (trap 1) and **`vmrange` histograms** (trap 3).
- **kubectl exec plugins hang** rather than fail (§6.2).
- **An unknown kubectl verb is not an error, it is a plugin.** `kubectl foo`
  execs `kubectl-foo` off `PATH`. The verb allowlist is therefore also the
  defence against arbitrary code execution, not only against writes (§6.1).
- **Log bodies are the highest-PII surface in the product.** Everything else
  drover holds is code, schemas and request definitions. This is the one place
  where a careless grep result in a transcript is a real incident, and it is
  the reason `redact` is on `LogSource` and not only in the ledger.

## 12. Still open

1. **Does the hot tier need auth?** Documented as a private port with none. If
   there is a header, it is `spec.tiers[].headers` with `${ENV}` values and the
   existing secret rules — but it must be checked in phase 1, not assumed.
2. **Should `logs search` on the hot tier also write its hits into the archive
   window directory** when one is already hydrated? It is what makes §2's point
   3 automatic. Recommendation: yes, into `lines.log` merged by event time,
   with the tier recorded per line — but it makes the file no longer a faithful
   copy of the archive, so the tier tag has to be in the line format from the
   start.
3. **A fourth tier for metrics-as-files?** Rendering a PromQL range result as a
   TSV in the tree would make `grep` work on it too. Probably over-reach; the
   answer to a metrics query is small enough to return inline.
4. **`covers` could be checked rather than declared** — sample the hot tier and
   report the severities actually present, the way §5.2 samples the archive. It
   would catch the day someone changes the collector filter and nobody updates
   the yaml. Cheap. Worth doing in phase 2 once the sampler exists.
