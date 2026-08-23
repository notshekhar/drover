// One stored object: what `drover get` shows, plus the yaml, plus the tool
// calls that have touched it.
//
// The activity tab is the one the CLI cannot give you. "Is this repository
// earning its disk" is a question you answer by looking at whether anything
// ever reads it.

import { useQueryState } from "nuqs";
import { activity, object as fetchObject } from "../api.js";
import { relativeTime, shortRemote, stamp } from "../fmt.js";
import { useQuery } from "../hooks.js";
import { Link, href, useTitle } from "../router.jsx";
import { objectTabParser, openParser } from "../state.js";
import {
  Code,
  Empty,
  ErrorNote,
  Kv,
  Mark,
  Pill,
  Section,
  Segmented,
  Skeleton,
  repoMarkKind,
} from "../ui.jsx";
import { ActivityTable } from "../ActivityTable.jsx";

// KINDS maps a dashboard path to the API's plural and how the ledger refers
// to this kind of object. Repositories are recorded in `repository`;
// everything else in `object`.
export const KINDS = {
  repositories: { plural: "repositories", title: "repository", filterKey: "repository", reconciled: true },
  requests: { plural: "httprequests", title: "http request", filterKey: "object" },
  databases: { plural: "sqlconnections", title: "sql connection", filterKey: "object", reconciled: true },
  environments: { plural: "environments", title: "environment", filterKey: "object" },
};

export function ObjectPage({ kind, name, live }) {
  const spec = KINDS[kind];
  useTitle(spec.title, name);
  const [tab, setTab] = useQueryState("tab", objectTabParser);
  const [open, setOpen] = useQueryState("open", openParser);

  const obj = useQuery((signal) => fetchObject(spec.plural, name, signal), kind + ":" + name, {
    interval: live ? 6000 : 0,
  });

  // The engine filters by object name now. It used to be sieved in the
  // browser out of the newest fifty calls overall, which showed a busy
  // engine's objects no history at all.
  const filter = { [spec.filterKey]: name, limit: 100 };
  const calls = useQuery(
    (signal) => activity(filter, signal),
    "objcalls:" + kind + ":" + name,
    { interval: live ? 6000 : 0 },
  );

  const o = obj.data;
  if (!o) {
    return (
      <>
        <ErrorNote error={obj.error} onRetry={obj.reload} />
        {obj.error ? null : <Skeleton rows={8} />}
      </>
    );
  }

  const items = calls.data?.items || [];

  return (
    <>
      <div className="page-head">
        <span className="page-kind">{spec.title}</span>
        <h1>
          {kind === "repositories" ? <Mark kind={repoMarkKind(o.status)} title={o.status} /> : null}
          {o.name || name}
        </h1>
        {/* Only the kinds drover actually reconciles have a status worth
            showing. An HTTPRequest reads "pending" forever because nothing
            ever moves it on, which looks like a fault and is not one. */}
        {spec.reconciled && o.status ? <Pill tone={statusTone(o.status)}>{o.status}</Pill> : null}
      </div>

      {o.error ? <div className="notice is-err">{o.error}</div> : null}

      <Segmented
        label="view"
        value={tab}
        onChange={(v) => setTab(v)}
        options={[
          { value: "overview", label: "overview" },
          { value: "yaml", label: "yaml" },
          { value: "activity", label: "activity (" + items.length + ")" },
        ]}
      />

      {tab === "overview" ? <Details kind={kind} o={o} /> : null}

      {tab === "yaml" ? (
        o.yaml ? (
          <Code text={o.yaml} />
        ) : (
          <Empty>this object was not stored with its source document</Empty>
        )
      ) : null}

      {tab === "activity" ? (
        <Section
          title="calls against this object"
          count={items.length}
          right={
            <Link
              to={
                href("activity") +
                "?" +
                spec.filterKey +
                "=" +
                encodeURIComponent(name)
              }
            >
              in the activity view
            </Link>
          }
        >
          <ErrorNote error={calls.error} onRetry={calls.reload} />
          {!calls.data ? (
            <Skeleton rows={5} />
          ) : items.length === 0 ? (
            <Empty hint="nothing has read it yet">no tool call has touched this</Empty>
          ) : (
            <ActivityTable items={items} open={open} onOpen={(id) => setOpen(id || null)} showDays />
          )}
        </Section>
      ) : null}
    </>
  );
}

function statusTone(status) {
  if (status === "failed") return "err";
  if (status === "ready" || status === "synced") return "ok";
  if (status === "syncing" || status === "pending") return "warn";
  return "dim";
}

function Details({ kind, o }) {
  if (kind === "repositories") {
    return (
      <>
        <Kv
          rows={[
            ["source", o.url ? <span title={o.url}>{shortRemote(o.url)}</span> : ""],
            ["branch", o.branch],
            ["refresh", o.refreshInterval],
            ["commit", o.commit],
            ["last sync", o.lastSync ? stamp(o.lastSync) + " (" + relativeTime(o.lastSync) + ")" : "never"],
            ["applied", o.appliedAt ? relativeTime(o.appliedAt) : ""],
            ["from", o.source],
          ]}
        />
      </>
    );
  }

  if (kind === "requests") {
    return (
      <>
        <Kv
          rows={[
            ["method", o.method],
            ["url", o.url],
            [
              "offered",
              o.safe ? (
                <span>
                  <Mark kind="on" /> yes, agents can call this
                </span>
              ) : (
                <span>
                  <Mark kind="off" /> no -- the method is not safe, so it is stored only
                </span>
              ),
            ],
            ["default environment", o.defaultEnvironment],
            ["environments", (o.environments || []).join(", ")],
            ["parameters", (o.params || []).join(", ")],
            ["applied", o.appliedAt ? relativeTime(o.appliedAt) : ""],
            ["from", o.source],
          ]}
        />
      </>
    );
  }

  if (kind === "databases") {
    return (
      <Kv
        rows={[
          ["provider", o.provider],
          [
            "access",
            o.readOnly ? "read-only" : <Pill tone="warn">writes allowed</Pill>,
          ],
          ["max rows", o.maxRows || ""],
          ["applied", o.appliedAt ? relativeTime(o.appliedAt) : ""],
          ["from", o.source],
        ]}
      />
    );
  }

  // Environments: the secret table is the whole point, and it says whether a
  // value is present without ever showing one.
  return (
    <>
      <Kv
        rows={[
          ["variables", (o.variables || []).join(", ")],
          ["applied", o.appliedAt ? relativeTime(o.appliedAt) : ""],
          ["from", o.source],
        ]}
      />
      {(o.secrets || []).length > 0 ? (
        <Section title="secrets" count={o.secrets.length}>
          <div className="table-wrap">
            <table className="rows">
              <thead>
                <tr>
                  <th className="col-mark" />
                  <th>name</th>
                  <th>reads</th>
                  <th>state</th>
                </tr>
              </thead>
              <tbody>
                {o.secrets.map((s) => (
                  <tr className="row" key={s.name}>
                    <td className="col-mark">
                      <Mark kind={s.set ? "on" : "fail"} title={s.set ? "set" : "unset"} />
                    </td>
                    <td>{s.name}</td>
                    <td className="dim">${s.fromEnv}</td>
                    <td className={s.set ? "dim" : "warn"}>
                      {s.set ? "set in the engine's environment" : "unset -- calls using it will fail"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Section>
      ) : null}
    </>
  );
}
