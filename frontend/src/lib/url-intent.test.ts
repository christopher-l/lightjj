import { describe, it, expect } from 'vitest'
import { parseUrlIntent, stripUrlIntent, isRevisionIdLike, locatorRevset, findRevisionIndexByRef, changeLink } from './url-intent'
import type { LogEntry } from './api'

describe('parseUrlIntent', () => {
  it.each([
    ['', null],
    ['?foo=bar', null],
    ['?change=', null],
    ['?change=%20%20', null],
    ['?change=wqnwkozp', { change: 'wqnwkozp' }],
    ['?change=WQNWkozp', { change: 'wqnwkozp' }],
    ['?change=%20abc12%20', { change: 'abc12' }],
    ['?revset=mine()', { revset: 'mine()' }],
    ['?revset=trunk()..%40', { revset: 'trunk()..@' }],
    ['?change=abc&revset=x%7Cy&path=src%2Fa.go', { change: 'abc', revset: 'x|y', path: 'src/a.go' }],
    ['?path=README.md', { path: 'README.md' }],
    // Raw pass-through: validity is App's isRevisionIdLike gate, so a bad value
    // can be surfaced as a warning instead of silently vanishing here.
    ['?change=main@origin', { change: 'main@origin' }],
  ])('%s → %j', (search, want) => {
    expect(parseUrlIntent(search)).toEqual(want)
  })
})

describe('stripUrlIntent', () => {
  it.each([
    ['', ''],
    ['?change=abc', ''],
    ['?change=abc&revset=x&path=p', ''],
    ['?debug=1&change=abc', '?debug=1'],
    ['?change=abc&keep=me&path=p', '?keep=me'],
  ])('%s → %s', (search, want) => {
    expect(stripUrlIntent(search)).toBe(want)
  })
})

describe('isRevisionIdLike', () => {
  it.each([
    ['wqnwkozp', true],     // change id (k–z)
    ['0123abcdef', true],   // commit id (hex)
    ['k', true], ['0', true],
    // Bookmark-ish words: mixed alphabets → neither a change id nor hex.
    ['main', false], ['abcxyz', false], ['cmid', false],
    ['ABC', false],         // parseUrlIntent lowercases; the gate itself is strict
    ['', false], ['a b', false], ['main@origin', false], ['trunk()', false], ['x|y', false], ['a-b', false],
  ])('%s → %s', (ref, want) => {
    expect(isRevisionIdLike(ref)).toBe(want)
  })
})

describe('locatorRevset', () => {
  it('wraps the ref and its context in present() and never uses all()', () => {
    const r = locatorRevset('abc123')
    expect(r).toBe('present(abc123) | present(@) | present(trunk())')
    expect(r).not.toContain('all()')
  })
})

describe('changeLink', () => {
  it('builds an origin-rooted ?change= URL that parseUrlIntent round-trips', () => {
    const url = changeLink('http://127.0.0.1:54321', 'wqnwkozp')
    expect(url).toBe('http://127.0.0.1:54321/?change=wqnwkozp')
    expect(parseUrlIntent(new URL(url).search)).toEqual({ change: 'wqnwkozp' })
  })
})

function row(change_id: string, commit_id: string, over: Partial<LogEntry['commit']> = {}): LogEntry {
  return {
    commit: {
      change_id, commit_id, change_prefix: 1, commit_prefix: 1,
      is_working_copy: false, hidden: false, immutable: false, conflicted: false,
      divergent: false, empty: false, mine: true, ...over,
    },
    description: '',
    graph_lines: [],
  }
}

describe('findRevisionIndexByRef', () => {
  const rows = [
    row('wqnwkozpaaaa', 'a1b2c3d4'),
    row('wqxyzzzzbbbb', 'a1ffee00'),
    row('kkkkkkkkcccc', '99887766'),
  ]

  it.each([
    ['wqnwkozpaaaa', 0],  // exact change_id (effectiveId)
    ['99887766', 2],      // exact commit_id
    ['wqn', 0],           // unique change_id prefix
    ['a1f', 1],           // unique commit_id prefix
    ['kk', 2],
    ['wq', -1],           // ambiguous across two changes
    ['a1', -1],           // ambiguous commit prefix
    ['nope', -1],
    ['', -1],
    // Longer-than-short refs (full ids from `jj log -T change_id/commit_id`):
    // the row's short id is a prefix of the ref.
    ['wqnwkozpaaaaqrstqrstqrstqrstqrst', 0],
    ['99887766' + '0'.repeat(32), 2],
    ['wqzzzzzzzzzzzzzzzzzz', -1],   // shares only 'wq' with two rows → no row id is its prefix
  ])('%s → %i', (ref, want) => {
    expect(findRevisionIndexByRef(rows, ref)).toBe(want)
  })

  it('resolves a divergent change_id to the first version in log order', () => {
    const div = [
      row('otherxxxxxxx', '11111111'),
      row('dvgdvgdvgdvg', '22222222', { divergent: true }),
      row('dvgdvgdvgdvg', '33333333', { divergent: true }),
    ]
    // effectiveId of a divergent row is its commit_id, so the change_id only
    // matches via the prefix path — both candidates share it → first wins.
    expect(findRevisionIndexByRef(div, 'dvgdvgdvgdvg')).toBe(1)
    expect(findRevisionIndexByRef(div, 'dvg')).toBe(1)
    // A specific version is still addressable by commit_id.
    expect(findRevisionIndexByRef(div, '33333333')).toBe(2)
    expect(findRevisionIndexByRef(div, '3333')).toBe(2)
  })
})
