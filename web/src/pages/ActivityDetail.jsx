// One call, in full. This is the page a link to a row lands on.

import { activityOne } from "../api.js";
import { bytes, millis, relativeTime, stamp } from "../fmt.js";
import { useQuery } from "../hooks.js";
import { Link, href, useTitle } from "../router.jsx";
import { Code, ErrorNote, Kv, Pill, Section, Skeleton, outcomeTone } from "../ui.jsx";

export function ActivityDetail({ id }) {
  const call = useQuery((signal) => activityOne(id, signal), "call:" + id);
  const r = call.data;
  useTitle(r ? (r.op ? r.tool + " " + r.op : r.tool) : "call");

  if (!r) {
    return (
      <>
        <ErrorNote error={call.error} onRetry={call.reload} />
        {call.error ? null : <Skeleton rows={6} />}
      </>
    );
  }

  const tool = r.op ? r.tool + " " + r.op : r.tool;

  return (
    <>
      <div className="page-head">
        <span className="page-kind">call</span>
        <h1>{tool}</h1>
        <Pill tone={outcomeTone(r.outcome)}>{r.outcome}</Pill>
      </div>

      {/* Self-reported, so it is quoted and kept away from the measurements. */}
      {r.reason ? <p className="reason-line">{'"' + r.reason + '"'}</p> : null}

      {r.error ? <div className="notice is-err">{r.error}</div> : null}

      <Kv
        rows={[
          ["summary", r.summary],
          ["at", stamp(r.at) + " (" + relativeTime(r.at) + ")"],
          ["duration", millis(r.durationMs)],
          ["bytes", bytes(r.bytes)],
          ["truncated", r.truncated ? "yes" : ""],
          ["source", r.source],
          ["client", r.client],
          [
            "session",
            r.session ? (
              <Link to={href("sessions", r.session)}>
                {r.session}
                {r.seqInSess ? <span className="dim"> call #{r.seqInSess}</span> : null}
              </Link>
            ) : (
              ""
            ),
          ],
          ["since previous", r.sincePrevMs ? millis(r.sincePrevMs) : ""],
          [
            "repository",
            r.repository ? (
              <Link to={href("repositories", r.repository)}>{r.repository}</Link>
            ) : (
              ""
            ),
          ],
          ["object", r.object],
          ["id", <span className="dim">{r.id}</span>],
        ]}
      />

      {r.args ? (
        <Section title="arguments">
          {/* As received, never as resolved: a resolved url can carry a token. */}
          <Code text={JSON.stringify(r.args, null, 2)} />
        </Section>
      ) : null}

      <div className="page-actions">
        {r.session ? (
          <Link className="btn" to={href("sessions", r.session)}>
            the whole session
          </Link>
        ) : null}
        <Link className="btn" to={href("activity") + "?tool=" + encodeURIComponent(r.tool)}>
          every {r.tool} call
        </Link>
        {r.repository ? (
          <Link
            className="btn"
            to={href("activity") + "?repository=" + encodeURIComponent(r.repository)}
          >
            everything against {r.repository}
          </Link>
        ) : null}
      </div>
    </>
  );
}
