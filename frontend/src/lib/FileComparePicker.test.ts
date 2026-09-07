import { describe, expect, it, vi, beforeEach, afterEach, type Mock } from 'vitest'
import { fireEvent, render } from '@testing-library/svelte'
import { tick } from 'svelte'

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  return {
    ...actual,
    api: { ...actual.api, fileHistory: vi.fn(), diffRange: vi.fn(), indexPaths: vi.fn() },
  }
})

import FileComparePicker from './FileComparePicker.svelte'
import { api, type LogEntry } from './api'
import { waitFor } from '../testutil/wait-for'

const mockHistory = api.fileHistory as Mock
const mockDiffRange = api.diffRange as Mock

function entry(id: string, desc: string): LogEntry {
  return {
    commit: {
      change_id: id, commit_id: `c_${id}`, change_prefix: 2, commit_prefix: 2,
      is_working_copy: false, hidden: false, immutable: false, conflicted: false,
      divergent: false, empty: false, mine: true,
      timestamp: '2026-03-20 10:00:00.000 +00:00',
    },
    description: desc,
    graph_lines: [],
  }
}
const REVS = [entry('abcdefgh', 'newest'), entry('ijklmnop', 'older')]
const DIFF = [
  'diff --git a/f.ts b/f.ts', '--- a/f.ts', '+++ b/f.ts',
  '@@ -1,1 +1,1 @@', '-old', '+new', '',
].join('\n')

const parentKeys = vi.fn()
beforeEach(() => {
  parentKeys.mockReset()
  document.addEventListener('keydown', parentKeys)
  mockHistory.mockReset(); mockDiffRange.mockReset()
  mockHistory.mockResolvedValue(REVS)
  mockDiffRange.mockResolvedValue({ diff: DIFF })
})

afterEach(() => document.removeEventListener('keydown', parentKeys))

async function mount() {
  // against = newest → row 0 is "the revision you're viewing"; row 1 diffs.
  // Listen ABOVE the mount target: Svelte 5 delegates on* handlers to the
  // mount root, so stopPropagation there is what keeps keys from App's
  // window-level router. A document listener observes exactly that.
  const r = render(FileComparePicker, { props: { path: 'f.ts', against: 'c_abcdefgh', onclose: vi.fn() } })
  await tick(); await tick()
  return { ...r, parentKeys }
}

describe('FileComparePicker', () => {
  // #38: split toggle is picker-local; `|` toggles it and must NOT bubble to
  // App (the picker isn't in anyModalOpen — a leaked key would drive the graph).
  it("'|' and the header button toggle split view; the key doesn't bubble", async () => {
    vi.useFakeTimers()
    let r: Awaited<ReturnType<typeof mount>>
    try {
      r = await mount()
      await fireEvent.keyDown(r.container.querySelector('.fcp-root')!, { key: 'j' })
      await tick()
      vi.advanceTimersByTime(50)
    } finally { vi.useRealTimers() }
    const { container, parentKeys } = r
    await waitFor(() => container.querySelector('.diff-file') !== null)
    expect(container.querySelector('.split-view')).toBeNull()
    parentKeys.mockClear()
    await fireEvent.keyDown(container.querySelector('.fcp-root')!, { key: '|' })
    expect(container.querySelector('.split-view')).not.toBeNull()
    expect(parentKeys).not.toHaveBeenCalled()
    await fireEvent.click(container.querySelector('.panel-actions .btn')!)
    expect(container.querySelector('.split-view')).toBeNull()
  })

  it('modifier chords (Cmd+|, Ctrl+j) are left to the browser/App, not consumed', async () => {
    const { container, parentKeys } = await mount()
    parentKeys.mockClear()
    await fireEvent.keyDown(container.querySelector('.fcp-root')!, { key: '|', metaKey: true })
    expect(container.querySelector('.split-view')).toBeNull()
    expect(parentKeys).toHaveBeenCalledTimes(1) // bubbled, untouched
  })
})
