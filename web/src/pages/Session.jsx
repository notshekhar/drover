// One session, in order.
//
// This is the view the ledger exists for. A burst of four calls in two
// seconds is one thought; a ninety-second pause is the model going away to
// write something. Read top to bottom it is the reasoning trace, which the
// agent's own transcript cannot give you because the transcript does not say
// what came back.

import { useQueryState } from "nuqs";
import { activity } from "../api.js";
import { millis, relativeTime, stamp } from "../fmt.js";
import { useQuery } from "../hooks.js";
import { Link, href, useTitle } from "../router.jsx";
import { openParser } from "../state.js";
import { Empty, ErrorNote, Kv, Section, Skeleton } from "../ui.jsx";
import { ActivityTable } from "../ActivityTable.jsx";

export function Session({ id, live }) {
  useTitle("session", id);
  const [open, setOpen] = useQueryState("open", openParser);
  const q = useQuery((signal) => activity({ session: id, limit: 1000 }, signal), "sess:" + id, {
    interval: live ? 5000 : 0,
  });

  if (!q.data) {
    return (
      <>
        <ErrorNote error={q.error} onRetry={q.reload} />
        {q.error ? null : <Skeleton rows={8} />}
      </>
    );
  }

  // The engine answers newest first; a session reads forwards.
  const items = (q.data.items || []).slice().reverse();
  if (items.length === 0) {
    return (
      <>
        <div className="page-head">
          <span className="page-kind">session</span>
          <h1>{id}</h1>
        </div>
        <Empty>no calls in this session</Empty>
      </>
    );
  }

  const first = items[0];
  const last = items[items.length - 1];
  const span = new Date(last.at).getTime() - new Date(first.at).getTime();
  const worked = items.reduce((n, r) => n + (r.durationMs || 0), 0);
  const errors = items.filter((r) => r.outcome === "error").length;
  const empties = items.filter((r) => r.outcome === "empty").length;
  const tools = [...new Set(items.map((r) => r.tool))];

  return (
    <>
      <div className="page-head">
        <span className="page-kind">session</span>
        <h1>{first.client || first.source || id}</h1>
        <span className="dim">{id}</span>
      </div>

      <Kv
        rows={[
          ["calls", items.length],
          ["span", span > 0 ? millis(span) : "instant"],
          ["engine time", millis(worked)],
          ["started", stamp(first.at) + " (" + relativeTime(first.at) + ")"],
          ["last call", relativeTime(last.at)],
          ["tools", tools.join(", ")],
          ["errors", errors ? <span className="err">{errors}</span> : ""],
          ["empty results", empties ? <span className="warn">{empties}</span> : ""],
        ]}
      />

      <Section
        title="the chain"
        count={items.length}
        right={
          <Link to={href("activity") + "?session=" + encodeURIComponent(id)}>
            in the activity view
          </Link>
        }
      >
        <ActivityTable items={items} open={open} onOpen={(id) => setOpen(id || null)} showGaps sticky />
      </Section>
    </>
  );
}
