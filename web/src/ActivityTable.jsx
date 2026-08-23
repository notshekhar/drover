// The activity table. Every view of the log is this component: the overview
// strip, the full activity page, a session chain, an object's own history.
//
// A row is a claim about one tool call. The self-reported `reason` is shown
// in quotes and never anywhere it could be mistaken for something drover
// measured.

import { Fragment } from "react";
import { Link, href } from "./router.jsx";
import { bytes, clock, dayOf, millis, relativeTime, stamp } from "./fmt.js";
import { Bar, Empty, Pill, outcomeTone } from "./ui.jsx";

export function ActivityTable({
  items,
  open,
  onOpen,
  onFilter,
  slowestMs,
  showDays = false,
  showGaps = false,
  dense = false,
  sticky = false,
}) {
  if (!items || items.length === 0) {
    return <Empty>no tool calls here</Empty>;
  }
  // A bar is a comparison, so it needs something to compare against. With
  // one or two rows the longest call fills the column and says nothing.
  const max =
    items.length < 3 ? 0 : slowestMs || items.reduce((m, r) => Math.max(m, r.durationMs || 0), 0);

  let lastDay = null;
  return (
    <div className={"table-wrap" + (sticky ? " is-sticky" : "")}>
      <table className={"rows" + (dense ? " is-dense" : "")}>
        <thead>
          <tr>
            <th className="col-time">time</th>
            <th className="col-tool">tool</th>
            <th className="col-target">target</th>
            <th className="col-summary">summary</th>
            <th className="col-source">source</th>
            <th className="col-ms">ms</th>
          </tr>
        </thead>
        <tbody>
          {items.map((r) => {
            const day = showDays ? dayOf(r.at) : null;
            const newDay = showDays && day !== lastDay;
            if (showDays) lastDay = day;
            return (
              <Fragment key={r.id}>
                {newDay ? (
                  <tr className="day">
                    <td colSpan={6}>{day}</td>
                  </tr>
                ) : null}
                <Row
                  r={r}
                  max={max}
                  open={open === r.id}
                  onOpen={onOpen}
                  onFilter={onFilter}
                  showGaps={showGaps}
                />
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function Row({ r, max, open, onOpen, onFilter, showGaps }) {
  const tool = r.op ? r.tool + " " + r.op : r.tool;
  const target = r.repository || r.object || "";
  // A gap longer than five seconds is the model going away to think. Marking
  // it is what turns a flat log into something with a shape.
  const gap = showGaps && r.sincePrevMs > 5000;

  const toggle = (e) => {
    if (!onOpen) return;
    // A click on a link inside the row is a navigation, not an expand.
    if (e.target.closest("a,button")) return;
    onOpen(open ? "" : r.id);
  };

  return (
    <>
      {gap ? (
        <tr className="gap">
          <td colSpan={6}>
            <span className="gap-line" />
            <span className="gap-label">{millis(r.sincePrevMs)} later</span>
          </td>
        </tr>
      ) : null}
      <tr
        className={"row" + (open ? " is-open" : "") + (r.outcome === "error" ? " is-err" : "")}
        onClick={toggle}
        tabIndex={onOpen ? 0 : undefined}
        onKeyDown={(e) => {
          if (!onOpen) return;
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onOpen(open ? "" : r.id);
          }
        }}
      >
        <td className="col-time dim" title={stamp(r.at)}>
          {clock(r.at)}
        </td>
        <td className="col-tool">
          <button
            type="button"
            className="linky"
            title={"filter to " + r.tool}
            onClick={(e) => {
              e.stopPropagation();
              onFilter ? onFilter({ tool: r.tool }) : onOpen?.(r.id);
            }}
          >
            {tool}
          </button>
        </td>
        <td className="col-target">
          {r.repository ? (
            <Link to={href("repositories", r.repository)} title={"repository " + r.repository}>
              {target}
            </Link>
          ) : (
            <span className="dim">{target || "—"}</span>
          )}
        </td>
        <td className="col-summary">
          <span className={r.outcome === "error" ? "err" : r.outcome === "empty" ? "warn" : ""}>
            {r.summary || r.error || r.outcome}
          </span>
          {r.truncated ? <Pill tone="warn">truncated</Pill> : null}
          {r.reason ? <span className="reason">{'"' + r.reason + '"'}</span> : null}
        </td>
        <td className="col-source dim">
          {r.session ? (
            <Link to={href("sessions", r.session)} title={"session " + r.session}>
              {r.client || r.source || "unknown"}
            </Link>
          ) : (
            r.client || r.source || ""
          )}
        </td>
        <td className="col-ms">
          <Bar value={r.durationMs} max={max} />
          <span className="ms-value">{r.durationMs != null ? r.durationMs : ""}</span>
        </td>
      </tr>
      {open ? <Detail r={r} onFilter={onFilter} /> : null}
    </>
  );
}

// The expanded row. Everything the list had no column for, without leaving
// the page you were reading.
function Detail({ r, onFilter }) {
  const facts = [
    ["outcome", <Pill tone={outcomeTone(r.outcome)}>{r.outcome}</Pill>],
    ["at", stamp(r.at) + " (" + relativeTime(r.at) + ")"],
    ["duration", millis(r.durationMs)],
    ["bytes", bytes(r.bytes)],
    ["source", r.source],
    ["client", r.client],
    ["seq", r.seqInSess ? "#" + r.seqInSess + " in session" : ""],
    ["since previous", r.sincePrevMs ? millis(r.sincePrevMs) : ""],
    ["object", r.object],
  ].filter(([, v]) => v !== null && v !== undefined && v !== "");

  return (
    <tr className="detail">
      <td colSpan={6}>
        <div className="detail-body">
          {r.error ? <p className="detail-error">{r.error}</p> : null}
          <dl className="detail-facts">
            {facts.map(([k, v]) => (
              <div key={k}>
                <dt>{k}</dt>
                <dd>{v}</dd>
              </div>
            ))}
          </dl>
          {r.args ? (
            <pre className="detail-args">{JSON.stringify(r.args, null, 2)}</pre>
          ) : null}
          <div className="detail-actions">
            <Link className="btn" to={href("activity", r.id)}>
              full record
            </Link>
            {r.session ? (
              <Link className="btn" to={href("sessions", r.session)}>
                the whole session
              </Link>
            ) : null}
            {onFilter && r.repository ? (
              <button
                type="button"
                className="btn"
                onClick={() => onFilter({ repository: r.repository })}
              >
                only {r.repository}
              </button>
            ) : null}
            {onFilter ? (
              <button type="button" className="btn" onClick={() => onFilter({ tool: r.tool })}>
                only {r.tool}
              </button>
            ) : null}
          </div>
        </div>
      </td>
    </tr>
  );
}
