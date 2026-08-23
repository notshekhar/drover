import { useLayoutEffect, useRef } from "react";
import { useQueryState } from "nuqs";
import { dashboard } from "./api.js";
import { humanDuration } from "./fmt.js";
import { useQuery } from "./hooks.js";
import { BASE, Link, href, useSegments, useTitle } from "./router.jsx";
import { liveParser } from "./state.js";
import { Empty } from "./ui.jsx";
import { Activity } from "./pages/Activity.jsx";
import { ActivityDetail } from "./pages/ActivityDetail.jsx";
import { KINDS, ObjectPage } from "./pages/ObjectPage.jsx";
import { Overview } from "./pages/Overview.jsx";
import { Session } from "./pages/Session.jsx";

export function App() {
  const parts = useSegments();
  const [live, setLive] = useQueryState("live", liveParser);

  // One dashboard request for the whole shell: the header wants the version
  // and the uptime, and the overview wants the rest of the same payload.
  const engine = useQuery((signal) => dashboard(signal), "engine", {
    interval: live ? 4000 : 0,
  });

  // The top bar grows a breadcrumb line on detail pages, so its height is
  // measured rather than guessed: it is what a sticky table header has to
  // clear, and a guess would leave rows sliding under the bar.
  const topRef = useRef(null);
  useLayoutEffect(() => {
    const el = topRef.current;
    if (!el) return;
    const apply = () =>
      document.documentElement.style.setProperty("--top-h", el.offsetHeight + "px");
    apply();
    const ro = new ResizeObserver(apply);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  return (
    <div className="shell">
      <Header
        topRef={topRef}
        parts={parts}
        engine={engine.data}
        stale={!!engine.error}
        live={live}
        onLive={() => setLive(!live)}
      />
      <main className="main">
        <Route parts={parts} live={live} engine={engine} />
      </main>
    </div>
  );
}

function Route({ parts, live, engine }) {
  if (parts.length === 0) return <Overview live={live} engine={engine} />;
  if (parts[0] === "activity" && parts[1]) return <ActivityDetail id={parts[1]} />;
  if (parts[0] === "activity") return <Activity live={live} />;
  if (parts[0] === "sessions" && parts[1]) return <Session id={parts[1]} live={live} />;
  if (KINDS[parts[0]] && parts[1]) {
    return <ObjectPage kind={parts[0]} name={parts[1]} live={live} />;
  }
  return <NotFound />;
}

function NotFound() {
  useTitle("not found");
  return (
    <Empty hint={<Link to={BASE + "/"}>back to the overview</Link>}>
      there is no page at this address
    </Empty>
  );
}

function Header({ topRef, parts, engine, stale, live, onLive }) {
  const section = parts[0] || "overview";
  const isActivity = section === "activity" || section === "sessions";

  return (
    <header className="top" ref={topRef}>
      <div className="top-inner">
        <Link to={BASE + "/"} className="brand">
          drover
          {engine?.version ? <span className="ver">{engine.version}</span> : null}
        </Link>

        <nav className="nav">
          <Link to={BASE + "/"} className={section === "overview" ? "is-current" : ""}>
            overview
          </Link>
          <Link to={href("activity")} className={isActivity ? "is-current" : ""}>
            activity
          </Link>
        </nav>

        <div className="top-right">
          {engine ? (
            <span className="dim" title="engine uptime">
              up {humanDuration(engine.uptimeSec)}
            </span>
          ) : null}
          <button
            type="button"
            className={"live" + (live ? " is-on" : "") + (stale ? " is-stale" : "")}
            onClick={onLive}
            title={
              stale
                ? "the engine is not answering"
                : live
                  ? "polling -- click to hold the view still"
                  : "paused -- click to follow new calls"
            }
            aria-pressed={live}
          >
            <span className="live-dot" />
            {stale ? "no engine" : live ? "live" : "paused"}
          </button>
        </div>
      </div>
      <Crumbs parts={parts} />
    </header>
  );
}

// Breadcrumbs, only where they say something the nav does not: on a call, a
// session or an object page you want a way back to the list you came from.
function Crumbs({ parts }) {
  if (parts.length < 2) return null;
  const [head, tail] = parts;
  const label = {
    activity: "activity",
    sessions: "sessions",
    repositories: "repositories",
    requests: "http requests",
    databases: "sql connections",
    environments: "environments",
  }[head];
  if (!label) return null;

  const backTo =
    head === "activity" || head === "sessions" ? href("activity") : BASE + "/";

  return (
    <div className="crumbs">
      <Link to={backTo}>{label}</Link>
      <span className="crumb-sep">/</span>
      <span className="crumb-current">{tail}</span>
    </div>
  );
}
