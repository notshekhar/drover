// The overview: what the engine is holding, and whether any of it is broken.
//
// The order is the point. Problems first, because a failing repository is the
// only thing on this page that needs a decision. Then the counts, then the
// tables, then the last few tool calls -- which is the part that makes the
// page worth leaving open.

import { Fragment } from "react";
import { useQueryStates } from "nuqs";
import { activity } from "../api.js";
import { humanDuration, relativeTime, shortCommit, shortRemote } from "../fmt.js";
import { useQuery } from "../hooks.js";
import { Link, href, useTitle } from "../router.jsx";
import { overviewParsers } from "../state.js";
import {
  Copyable,
  Empty,
  ErrorNote,
  Field,
  Mark,
  Pill,
  Section,
  Skeleton,
  repoMarkKind,
  useSearchFocus,
} from "../ui.jsx";
import { ActivityTable } from "../ActivityTable.jsx";

// The engine state comes in as a prop: the shell is already polling it for
// the version and uptime in the header, and one dashboard is one request.
export function Overview({ live, engine }) {
  useTitle("overview");
  const [{ q }, setQ] = useQueryStates(overviewParsers);
  const searchRef = useSearchFocus();

  const recent = useQuery((signal) => activity({ limit: 12 }, signal), "recent", {
    interval: live ? 4000 : 0,
  });

  const state = engine;
  const d = state.data;

  if (!d) {
    return (
      <>
        <ErrorNote error={state.error} onRetry={state.reload} />
        {state.error ? null : <Skeleton rows={10} />}
      </>
    );
  }

  const repos = d.repos || [];
  const reqs = d.requests || [];
  const sqls = d.sqls || [];
  const envs = d.envs || [];

  const hit = (...fields) => {
    if (!q) return true;
    const needle = q.toLowerCase();
    return fields.some((f) => String(f || "").toLowerCase().includes(needle));
  };

  const shownRepos = repos.filter((r) => hit(r.name, r.url, r.branch, r.status));
  const shownReqs = reqs.filter((r) => hit(r.name, r.url, r.method, r.environment));
  const shownSqls = sqls.filter((s) => hit(s.name, s.provider, s.status));
  const shownEnvs = envs.filter((e) => hit(e.name));

  const problems = [
    ...repos
      .filter((r) => r.status === "failed")
      .map((r) => ({ to: href("repositories", r.name), name: r.name, text: r.error || "sync failed" })),
    ...sqls
      .filter((s) => s.status && s.status !== "ready")
      .map((s) => ({ to: href("databases", s.name), name: s.name, text: s.error || s.status })),
    ...envs
      .filter((e) => e.unset > 0)
      .map((e) => ({
        to: href("environments", e.name),
        name: e.name,
        text: e.unset + " secret(s) unset -- calls using this will fail",
      })),
  ];

  return (
    <>
      <ErrorNote error={state.error} onRetry={state.reload} />

      <div className="engine">
        <div className="engine-facts">
          <EngineFact label="engine" value={d.listen ? "http://" + d.listen : "-"} copy />
          <EngineFact label="mcp" value={d.listen ? "http://" + d.listen + "/mcp" : "-"} copy />
          <EngineFact label="data" value={d.dataDir || "-"} copy />
          <EngineFact label="uptime" value={humanDuration(d.uptimeSec)} />
        </div>
      </div>

      {problems.length > 0 ? (
        <div className="problems" role="alert">
          <h2>
            {problems.length === 1 ? "1 thing needs attention" : problems.length + " things need attention"}
          </h2>
          <ul>
            {problems.map((p) => (
              <li key={p.to + p.text}>
                <Mark kind="fail" />
                <Link to={p.to}>{p.name}</Link>
                <span className="problem-text">{p.text}</span>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <div className="tiles">
        <Tile
          label="repositories"
          count={repos.length}
          bad={repos.filter((r) => r.status === "failed").length}
          badLabel="failing"
          okLabel="synced"
        />
        <Tile
          label="http requests"
          count={reqs.length}
          note={reqs.filter((r) => r.offered).length + " offered to agents"}
        />
        <Tile
          label="databases"
          count={sqls.length}
          bad={sqls.filter((s) => s.status && s.status !== "ready").length}
          badLabel="unreachable"
          okLabel="ready"
        />
        <Tile
          label="environments"
          count={envs.length}
          bad={envs.reduce((n, e) => n + (e.unset || 0), 0)}
          badLabel="secrets unset"
          okLabel="resolved"
        />
      </div>

      <div className="overview-search">
        <Field
          label="filter"
          value={q}
          onChange={(v) => setQ({ q: v || null })}
          placeholder="name, url, provider          /"
          inputRef={searchRef}
          wide
        />
        {q ? (
          <span className="dim">
            {shownRepos.length + shownReqs.length + shownSqls.length + shownEnvs.length} of{" "}
            {repos.length + reqs.length + sqls.length + envs.length} objects
          </span>
        ) : null}
      </div>

      <Repos repos={shownRepos} total={repos.length} filtered={!!q} />
      <Requests reqs={shownReqs} total={reqs.length} filtered={!!q} />
      <Databases sqls={shownSqls} total={sqls.length} filtered={!!q} />
      <Envs envs={shownEnvs} total={envs.length} filtered={!!q} />

      <Section
        title="recent calls"
        count={(recent.data?.items || []).length}
        right={<Link to={href("activity")}>all activity</Link>}
      >
        <ErrorNote error={recent.error} onRetry={recent.reload} />
        {recent.data ? (
          <ActivityTable items={recent.data.items || []} dense />
        ) : (
          <Skeleton rows={5} />
        )}
      </Section>
    </>
  );
}

function EngineFact({ label, value, copy }) {
  return (
    <div className="engine-fact">
      <span className="engine-label">{label}</span>
      {copy ? <Copyable text={value} /> : <span className="engine-value">{value}</span>}
    </div>
  );
}

// A tile is a count and its one worrying number. When nothing is wrong it
// says so quietly rather than going green and loud.
function Tile({ label, count, bad = 0, badLabel, okLabel, note }) {
  return (
    <div className={"tile" + (bad ? " is-bad" : "")}>
      <span className="tile-label">{label}</span>
      <span className="tile-count">{count}</span>
      <span className="tile-note">
        {count === 0 ? "none" : bad ? bad + " " + badLabel : note || okLabel || ""}
      </span>
    </div>
  );
}

function emptyOr(list, total, filtered, hint) {
  if (list.length > 0) return null;
  if (filtered && total > 0) return <Empty>nothing matches the filter</Empty>;
  return <Empty hint={hint}>none yet</Empty>;
}

function Repos({ repos, total, filtered }) {
  return (
    <Section title="repositories" count={filtered ? repos.length + "/" + total : total}>
      {emptyOr(repos, total, filtered, "drover apply -f repo.yaml") || (
        <div className="table-wrap">
          <table className="rows">
            <thead>
              <tr>
                <th className="col-mark" />
                <th>name</th>
                <th>source</th>
                <th>branch</th>
                <th>commit</th>
                <th>refresh</th>
                <th>synced</th>
              </tr>
            </thead>
            <tbody>
              {repos.map((r) => (
                <Fragment key={r.name}>
                  <tr className="row">
                    <td className="col-mark">
                      <Mark kind={repoMarkKind(r.status)} title={r.status || "pending"} />
                    </td>
                    <td>
                      <Link to={href("repositories", r.name)}>{r.name}</Link>
                    </td>
                    <td className="dim">{shortRemote(r.url)}</td>
                    <td>{r.branch || ""}</td>
                    <td className="dim">{shortCommit(r.commit)}</td>
                    <td className="dim">{r.refresh || ""}</td>
                    <td className="dim">{relativeTime(r.lastSync)}</td>
                  </tr>
                  {r.error ? (
                    <tr className="row-note">
                      <td />
                      <td colSpan={6} className="err">
                        {r.error}
                      </td>
                    </tr>
                  ) : null}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  );
}

function Requests({ reqs, total, filtered }) {
  return (
    <Section title="http requests" count={filtered ? reqs.length + "/" + total : total}>
      {emptyOr(reqs, total, filtered, "an HTTPRequest object turns an API into a tool") || (
        <div className="table-wrap">
          <table className="rows">
            <thead>
              <tr>
                <th className="col-mark" />
                <th>name</th>
                <th>method</th>
                <th>environment</th>
                <th>url</th>
              </tr>
            </thead>
            <tbody>
              {reqs.map((r) => (
                <tr className="row" key={r.name}>
                  <td className="col-mark">
                    <Mark
                      kind={r.offered ? "on" : "off"}
                      title={r.offered ? "offered to agents" : "stored, not offered (unsafe method)"}
                    />
                  </td>
                  <td>
                    <Link to={href("requests", r.name)}>{r.name}</Link>
                  </td>
                  <td>{r.method || ""}</td>
                  <td className="dim">{r.environment || "-"}</td>
                  <td className="dim url">{r.url || ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  );
}

function Databases({ sqls, total, filtered }) {
  return (
    <Section title="sql connections" count={filtered ? sqls.length + "/" + total : total}>
      {emptyOr(sqls, total, filtered, "a SQLConnection object is read-only unless you say otherwise") || (
        <div className="table-wrap">
          <table className="rows">
            <thead>
              <tr>
                <th className="col-mark" />
                <th>name</th>
                <th>provider</th>
                <th>access</th>
                <th>status</th>
              </tr>
            </thead>
            <tbody>
              {sqls.map((s) => {
                const ready = s.status === "ready";
                return (
                  <Fragment key={s.name}>
                    <tr className="row">
                      <td className="col-mark">
                        <Mark kind={ready ? "on" : "fail"} title={s.status} />
                      </td>
                      <td>
                        <Link to={href("databases", s.name)}>{s.name}</Link>
                      </td>
                      <td className="dim">{s.provider || ""}</td>
                      <td>
                        {s.readOnly ? (
                          <span className="dim">read-only</span>
                        ) : (
                          <Pill tone="warn">writes allowed</Pill>
                        )}
                      </td>
                      <td>
                        <Pill tone={ready ? "ok" : "err"}>{s.status || "unknown"}</Pill>
                      </td>
                    </tr>
                    {s.error ? (
                      <tr className="row-note">
                        <td />
                        <td colSpan={4} className="err">
                          {s.error}
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  );
}

function Envs({ envs, total, filtered }) {
  if (total === 0) return null;
  return (
    <Section title="environments" count={filtered ? envs.length + "/" + total : total}>
      {emptyOr(envs, total, filtered) || (
        <div className="table-wrap">
          <table className="rows">
            <thead>
              <tr>
                <th>name</th>
                <th>variables</th>
                <th>secrets</th>
              </tr>
            </thead>
            <tbody>
              {envs.map((e) => (
                <tr className="row" key={e.name}>
                  <td>
                    <Link to={href("environments", e.name)}>{e.name}</Link>
                  </td>
                  <td className="dim">{e.variables}</td>
                  <td className={e.unset ? "warn" : "dim"}>
                    {e.unset ? e.secrets + " (" + e.unset + " unset)" : e.secrets}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  );
}
