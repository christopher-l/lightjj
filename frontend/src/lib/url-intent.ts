// PURE — routable-URL intent (issue #35). An agent that made changes can hand
// the user `http://<addr>/?change=<id>` (optionally `&revset=…`, `&path=…`);
// AppShell parses it ONCE at boot, seeds the launch tab's first App mount, and
// strips the params so a browser refresh returns to the base URL.
//
// Grammar (all optional, at least one present):
//   change=<change_id | commit_id>   full or unique prefix — select that revision
//   revset=<revset>                  set the revset filter (as if typed)
//   path=<repo-relative file>        scroll the selected revision's diff to it

import { effectiveId, type LogEntry } from './api'

export interface UrlIntent {
  change?: string
  revset?: string
  path?: string
}

export const URL_INTENT_PARAMS = ['change', 'revset', 'path'] as const

/** Parse `location.search`. Values are trimmed; empties dropped; null when no
 *  intent param is present. `change` is lowercased (jj ids are) but otherwise
 *  passed through raw — App validates with isRevisionIdLike before it ever
 *  builds a revset from it, so a bad value surfaces as a warning, not silence. */
export function parseUrlIntent(search: string): UrlIntent | null {
  const q = new URLSearchParams(search)
  const out: UrlIntent = {}
  const change = q.get('change')?.trim().toLowerCase()
  const revset = q.get('revset')?.trim()
  const path = q.get('path')?.trim()
  if (change) out.change = change
  if (revset) out.revset = revset
  if (path) out.path = path
  return Object.keys(out).length > 0 ? out : null
}

/** `search` with the intent params removed and everything else preserved —
 *  '' when nothing remains, else '?a=b'. Feed to history.replaceState. */
export function stripUrlIntent(search: string): string {
  const q = new URLSearchParams(search)
  for (const k of URL_INTENT_PARAMS) q.delete(k)
  const s = q.toString()
  return s ? `?${s}` : ''
}

/** The shareable inverse of parseUrlIntent: `<origin>/?change=<id>`. Built on
 *  location.origin + '/' (not href) so a link copied from a non-launch tab
 *  still lands on the app root — routing is launch-tab-only by design. */
export function changeLink(origin: string, changeId: string): string {
  return `${origin}/?${new URLSearchParams({ change: changeId })}`
}

/** A change id (jj's reversed-hex k–z alphabet) or a commit id (hex) — the two
 *  disjoint alphabets, lowercase (parseUrlIntent lowercases). The gate before
 *  interpolating a ref into locatorRevset: `main` (a bookmark), `@`, or revset
 *  syntax fail it and get the "use ?revset=" warning instead of being resolved
 *  as a symbol via present(main). */
export function isRevisionIdLike(ref: string): boolean {
  return /^(?:[k-z]+|[0-9a-f]+)$/.test(ref)
}

/** The auto-widen revset for a ref that isn't in the loaded log: the ref plus
 *  just enough context (@, trunk) to orient — cheap on huge repos, never all().
 *  present() turns a NONEXISTENT ref into "not found" instead of a jj error —
 *  but NOT an ambiguous short prefix; that still errors, and App's loadLog
 *  error path restores the previous revset when it does. */
export function locatorRevset(ref: string): string {
  return `present(${ref}) | present(@) | present(trunk())`
}

/** Index of the log row a change/commit ref names, or -1. Exact effectiveId
 *  wins; else a UNIQUE prefix match against change_id or commit_id in EITHER
 *  direction (agents paste short prefixes; `jj log -T` prints ids longer than
 *  the log's 12-char short form). Several prefix candidates that are all the same change
 *  (divergent versions) resolve to the first in log order — jj's /0; any other
 *  ambiguity is a miss (widening can't disambiguate, so the caller warns). */
export function findRevisionIndexByRef(rows: readonly LogEntry[], ref: string): number {
  if (!ref) return -1
  const exact = rows.findIndex(r => effectiveId(r.commit) === ref)
  if (exact >= 0) return exact
  const hits: number[] = []
  // Bidirectional: the log carries jj's SHORT ids (12 chars), so a full
  // 32-char change id / 40-char commit id (what `jj log -T change_id` prints)
  // is matched as "row id is a prefix of the ref" — otherwise it would miss,
  // widen, resolve fine in jj, and still miss here.
  const pre = (id: string) => id.startsWith(ref) || (id.length > 0 && ref.startsWith(id))
  rows.forEach((r, i) => {
    if (pre(r.commit.change_id) || pre(r.commit.commit_id)) hits.push(i)
  })
  if (hits.length === 1) return hits[0]
  if (hits.length > 1 && hits.every(i => rows[i].commit.change_id === rows[hits[0]].commit.change_id)) return hits[0]
  return -1
}
