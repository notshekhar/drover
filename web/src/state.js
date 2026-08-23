// Every piece of view state the dashboard holds lives in the URL.
//
// The rule: if reloading the page or sending someone the link should show the
// same screen, it is a query parameter -- filters, sort, which row is open,
// which tab, whether the page is polling. nuqs is what keeps that honest;
// these parsers are where the names and defaults are agreed once so two pages
// cannot disagree about what `?outcome=` means.
//
// Defaults are cleared from the URL rather than written out, so an untouched
// page has a bare `/dashboard/activity` and every parameter you see is one
// somebody actually chose.

import { debounce, parseAsBoolean, parseAsInteger, parseAsString, parseAsStringLiteral } from "nuqs";

export const SORTS = ["recent", "slow"];
export const LIMITS = [50, 100, 250, 500, 1000];
export const DEFAULT_LIMIT = 100;

// A typed search box should not push a URL update per keystroke: the URL is
// also the fetch key, so that would be a request per keystroke too. Typing
// replaces rather than pushes for the same reason -- twenty history entries
// for one word would make the back button useless.
const searchText = parseAsString.withDefault("").withOptions({
  limitUrlUpdates: debounce(250),
});

// Everything else is a deliberate, discrete act: a chip, a sort, a tab, an
// opened row. Those push, so the back button undoes exactly the last thing
// you did instead of leaving the page entirely.
//
// It is declared on the parser rather than passed at each call site, so the
// rule lives with the parameter and no caller can forget it.
const push = { history: "push" };

const pushedString = parseAsString.withDefault("").withOptions(push);

// The activity filter, as the engine's /activity endpoint understands it.
// Sending these straight through is deliberate -- the query string in the
// browser and the query string on the wire are the same vocabulary.
export const activityParsers = {
  q: searchText,
  tool: pushedString,
  source: pushedString,
  outcome: pushedString,
  repository: pushedString,
  object: pushedString,
  session: pushedString,
  sort: parseAsStringLiteral(SORTS).withDefault("recent").withOptions(push),
  limit: parseAsInteger.withDefault(DEFAULT_LIMIT).withOptions(push),
};

// Which row is expanded. In the URL because "look at this call" is the most
// common thing anybody wants to send someone.
export const openParser = parseAsString.withDefault("").withOptions(push);

// Auto-refresh. Off is worth persisting: reading a log while it reorders
// under you is the one time a live dashboard is a hindrance.
export const liveParser = parseAsBoolean.withDefault(true);

// The overview's filter across every object table.
export const overviewParsers = { q: searchText };

export const objectTabParser = parseAsStringLiteral(["overview", "yaml", "activity"])
  .withDefault("overview")
  .withOptions(push);

// filterOf strips defaults and empties, so the request carries only what was
// chosen. `sort=recent` is the engine's default too; sending it would just be
// noise in the network tab.
export function filterOf(f, extra) {
  const out = { ...extra };
  for (const [k, v] of Object.entries(f)) {
    if (v === "" || v === null || v === undefined) continue;
    if (k === "sort" && v === "recent") continue;
    out[k] = v;
  }
  return out;
}

// countActive is what the "clear" button needs to know whether it has a job.
export function countActive(f) {
  return Object.entries(f).filter(([k, v]) => {
    if (k === "limit") return v !== DEFAULT_LIMIT;
    if (k === "sort") return v !== "recent";
    return v !== "" && v !== null && v !== undefined;
  }).length;
}
