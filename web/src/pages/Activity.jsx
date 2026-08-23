// The activity view: every tool call, filtered.
//
// The whole filter lives in the URL, which is the reason this page is worth
// having. "Every grep that came back empty against the api repo" is a link
// you can paste to someone, and it is the same query string the engine's own
// /activity endpoint takes -- what you see in the address bar is what went
// over the wire.

import { useCallback, useEffect, useState } from "react";
import { useQueryState, useQueryStates } from "nuqs";
import { activity, activityStats } from "../api.js";
import { millis } from "../fmt.js";
import { useQuery } from "../hooks.js";
import { useTitle } from "../router.jsx";
import { LIMITS, activityParsers, countActive, filterOf, openParser } from "../state.js";
import {
  Chip,
  Empty,
  ErrorNote,
  Field,
  Section,
  Segmented,
  Skeleton,
  useSearchFocus,
} from "../ui.jsx";
import { ActivityTable } from "../ActivityTable.jsx";

export function Activity({ live }) {
  const [f, setF] = useQueryStates(activityParsers);
  const [open, setOpen] = useQueryState("open", openParser);
  const searchRef = useSearchFocus();

  // Paging is local, not URL state: a cursor is where you got to in this
  // sitting, not what the link means. Sending someone `?before=<id>` would
  // hand them a page whose first row is already scrolling away.
  const [pages, setPages] = useState([]);
  const key = JSON.stringify(f);
  useEffect(() => {
    setPages([]);
  }, [key]);

  const params = filterOf(f);
  const head = useQuery((signal) => activity(params, signal), key, {
    // Live tail only makes sense at the head of the log. Once you have paged
    // down, rows arriving above you would shuffle the ground you stand on.
    interval: live && pages.length === 0 ? 3000 : 0,
  });
  const stats = useQuery((signal) => activityStats(params, signal), "stats:" + key, {
    interval: live && pages.length === 0 ? 6000 : 0,
  });

  useTitle("activity", countActive(f) ? "filtered" : "");

  const items = [...(head.data?.items || []), ...pages.flat()];
  const st = stats.data;
  const active = countActive(f);

  // Only the keys the click actually touched are written. Sending the whole
  // filter back would re-queue the debounced search box on every chip, which
  // costs a second URL write a quarter-second later for no change at all.
  const set = useCallback(
    (patch) => {
      setF((prev) => {
        const next = {};
        for (const [k, v] of Object.entries(patch)) {
          // Clicking the filter that is already on takes it off again, which
          // is what a chip that shows its own state has to do.
          next[k] = prev[k] === v && v !== "" ? null : v;
        }
        return next;
      });
    },
    [setF],
  );

  const loadMore = useCallback(async () => {
    const last = items[items.length - 1];
    if (!last) return;
    const more = await activity({ ...params, before: last.id });
    setPages((p) => [...p, more.items || []]);
  }, [items, params]);

  // The end is what the *last* fetch said, not what all of them said: a full
  // first page followed by a short one is still the end of the log.
  const lastPage = pages.length > 0 ? pages[pages.length - 1] : head.data?.items;
  const atEnd = !!head.data && (lastPage?.length ?? 0) < f.limit;

  return (
    <>
      <div className="filters">
        <div className="filters-row">
          <Field
            label="search"
            value={f.q}
            onChange={(v) => setF({ q: v || null })}
            placeholder="summary, reason, error, arguments          /"
            inputRef={searchRef}
            wide
          />
          <Segmented
            label="sort"
            value={f.sort}
            onChange={(v) => setF({ sort: v })}
            options={[
              { value: "recent", label: "newest" },
              { value: "slow", label: "slowest" },
            ]}
          />
          <label className="field is-small">
            <span className="field-label">rows</span>
            <select value={f.limit} onChange={(e) => setF({ limit: Number(e.target.value) })}>
              {LIMITS.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
          {active > 0 ? (
            <button type="button" className="btn is-quiet" onClick={() => setF(null)}>
              clear {active}
            </button>
          ) : null}
        </div>

        <div className="filters-row">
          <Field
            label="repository"
            value={f.repository}
            onChange={(v) => setF({ repository: v || null })}
            placeholder="api"
          />
          <Field
            label="object"
            value={f.object}
            onChange={(v) => setF({ object: v || null })}
            placeholder="get-user"
          />
          <Field
            label="session"
            value={f.session}
            onChange={(v) => setF({ session: v || null })}
            placeholder=""
          />
        </div>

        <Facets st={st} f={f} set={set} />
      </div>

      <Section
        title="calls"
        count={st ? st.total : items.length}
        right={
          st && st.slowestMs > 0 ? (
            <span className="dim">slowest in view {millis(st.slowestMs)}</span>
          ) : null
        }
      >
        <ErrorNote error={head.error} onRetry={head.reload} />
        {!head.data ? (
          <Skeleton rows={10} />
        ) : items.length === 0 ? (
          <Empty hint={active ? "clear the filters to see everything" : "run a tool through drover and it lands here"}>
            {active ? "no calls match this filter" : "no tool calls yet"}
          </Empty>
        ) : (
          <>
            <ActivityTable
              items={items}
              open={open}
              onOpen={(id) => setOpen(id || null)}
              onFilter={set}
              slowestMs={st?.slowestMs}
              showDays={f.sort === "recent"}
              sticky
            />
            <div className="more">
              {atEnd ? (
                <span className="dim">that is the whole log for this filter</span>
              ) : (
                <button type="button" className="btn" onClick={loadMore}>
                  load older
                </button>
              )}
            </div>
          </>
        )}
      </Section>
    </>
  );
}

// The facets are the filter bar's other half: what is actually in the set,
// counted, and clickable. Every chip is a filter you did not have to guess
// the spelling of.
function Facets({ st, f, set }) {
  if (!st) return <div className="facets is-loading" />;
  const groups = [
    { key: "outcome", label: "outcome", buckets: st.outcomes, tone: outcomeTone },
    { key: "tool", label: "tool", buckets: st.tools },
    { key: "source", label: "source", buckets: st.sources },
    { key: "repository", label: "repo", buckets: st.repositories },
  ].filter((g) => g.buckets && g.buckets.length > 0);

  if (groups.length === 0) return null;

  return (
    <div className="facets">
      {groups.map((g) => (
        <div className="facet" key={g.key}>
          <span className="facet-label">{g.label}</span>
          <div className="facet-chips">
            {g.buckets.map((b) => (
              <Chip
                key={b.value}
                active={f[g.key] === b.value}
                count={b.count}
                tone={g.tone ? g.tone(b.value) : null}
                onClick={() => set({ [g.key]: b.value })}
                title={g.label + " = " + b.value}
              >
                {b.value}
              </Chip>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

function outcomeTone(v) {
  if (v === "error") return "err";
  if (v === "empty") return "warn";
  return null;
}
