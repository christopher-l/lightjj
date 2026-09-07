import { describe, expect, it, vi, beforeEach, type Mock } from 'vitest'
import { fireEvent, render } from '@testing-library/svelte'
import { tick } from 'svelte'

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  return {
    ...actual,
    api: { ...actual.api, fileHistory: vi.fn(), diffRange: vi.fn(), indexPaths: vi.fn() },
  }
})

import FileHistoryPanel from './FileHistoryPanel.svelte'
import { api, type LogEntry } from './api'
import { waitFor } from '../testutil/wait-for'

const mockHistory = api.fileHistory as Mock
const mockDiffRange = api.diffRange as Mock
const mockIndexPaths = api.indexPaths as Mock

function entry(id: string, desc: string, immutable = false): LogEntry {
  return {
    commit: {
      change_id: id, commit_id: `c_${id}`, change_prefix: 2, commit_prefix: 2,
      is_working_copy: false, hidden: false, immutable, conflicted: false,
      divergent: false, empty: false, mine: true,
      timestamp: '2026-03-20 10:00:00.000 +00:00',
    },
    description: desc,
    graph_lines: [],
  }
}

const REVS = [
  entry('abcdefgh', 'newest change'),
  entry('ijklmnop', 'middle change'),
  entry('qrstuvwx', 'oldest change', true),
]

beforeEach(() => {
  mockHistory.mockReset()
  mockDiffRange.mockReset()
  mockIndexPaths.mockReset()
  mockHistory.mockResolvedValue(REVS)
  mockDiffRange.mockResolvedValue({ diff: '' })
  mockIndexPaths.mockResolvedValue(undefined)
})

async function mount(onclose = vi.fn(), initialPin: string | null = null) {
  const r = render(FileHistoryPanel, { props: { path: 'src/lib/api.ts', onclose, initialPin } })
  await tick() // history.load effect fires
  await tick() // loader resolves + renders
  return { ...r, onclose }
}


const kd = (c: ReturnType<typeof render>['component'], key: string) =>
  (c as { handleKeydown: (e: KeyboardEvent) => boolean }).handleKeydown(new KeyboardEvent('keydown', { key }))

describe('FileHistoryPanel', () => {
  it('fetches history for the given path on mount (mutable-scoped by default)', async () => {
    await mount()
    expect(mockHistory).toHaveBeenCalledWith('src/lib/api.ts', false)
  })

  it('j/k moves cursorB, clamps at bounds', async () => {
    const { component, container } = await mount()
    const cursorRow = () => container.querySelector('.fh-cursor')?.getAttribute('data-idx')

    expect(cursorRow()).toBe('0')
    kd(component, 'j'); await tick(); expect(cursorRow()).toBe('1')
    kd(component, 'j'); await tick(); expect(cursorRow()).toBe('2')
    kd(component, 'j'); await tick(); expect(cursorRow()).toBe('2') // clamp
    kd(component, 'k'); await tick(); expect(cursorRow()).toBe('1')
    kd(component, 'k'); await tick(); expect(cursorRow()).toBe('0')
    kd(component, 'k'); await tick(); expect(cursorRow()).toBe('0') // clamp
  })

  it('Space pins A at current cursorB position', async () => {
    const { component, container } = await mount()
    const pinnedRow = () => container.querySelector('.fh-pinned')?.getAttribute('data-idx')

    expect(pinnedRow()).toBe('0')
    kd(component, 'j')
    kd(component, 'j')
    kd(component, ' ')
    await tick()
    expect(pinnedRow()).toBe('2')
  })

  it('A===B shows empty-state message, no diffRange call', async () => {
    const { container } = await mount()
    expect(container.querySelector('.fh-empty-state')?.textContent).toContain('Same revision')
    expect(mockDiffRange).not.toHaveBeenCalled()
  })

  it('moving B away from A triggers diffRange(B→A) after 50ms debounce', async () => {
    vi.useFakeTimers()
    try {
      const { component } = await mount()
      kd(component, 'j')
      await tick()
      expect(mockDiffRange).not.toHaveBeenCalled()  // debounce not elapsed
      vi.advanceTimersByTime(50)
      // bug_001: from=B(older), to=A(newer) — green shows additions over time.
      expect(mockDiffRange).toHaveBeenCalledWith('c_ijklmnop', 'c_abcdefgh', ['src/lib/api.ts'])
    } finally {
      vi.useRealTimers()
    }
  })

  it('rapid j/k fires only ONE diffRange (debounce coalesces)', async () => {
    vi.useFakeTimers()
    try {
      const { component } = await mount()
      kd(component, 'j'); kd(component, 'j')  // B → index 2
      await tick()
      vi.advanceTimersByTime(50)
      expect(mockDiffRange).toHaveBeenCalledTimes(1)
      expect(mockDiffRange).toHaveBeenCalledWith('c_qrstuvwx', 'c_abcdefgh', ['src/lib/api.ts'])
    } finally {
      vi.useRealTimers()
    }
  })

  it('Escape calls onclose', async () => {
    const { component, onclose } = await mount()
    expect(kd(component, 'Escape')).toBe(true)
    expect(onclose).toHaveBeenCalledOnce()
  })

  it('row click sets cursorB', async () => {
    const { container } = await mount()
    const rows = container.querySelectorAll('.fh-row')
    await fireEvent.click(rows[2])
    expect(container.querySelector('.fh-cursor')?.getAttribute('data-idx')).toBe('2')
  })

  it('immutable rows are dimmed', async () => {
    const { container } = await mount()
    const rows = container.querySelectorAll('.fh-row')
    expect(rows[2].classList.contains('fh-immutable')).toBe(true)
    expect(rows[0].classList.contains('fh-immutable')).toBe(false)
  })

  // initialPin ("View history" from a revision): the rail must skip the
  // mutable tier entirely and pin A by commit_id in the FULL list. Previously
  // both tiers raced; the one-shot pin resolved an index against whichever
  // landed first, then the other list replaced it → A named a different commit.
  it('initialPin: skips the mutable tier and pins A by commit_id in the full list', async () => {
    // Mutable tier would be WIP-only; full has the immutable target at index 2.
    mockHistory.mockImplementation((_p: string, full: boolean) =>
      Promise.resolve(full ? REVS : REVS.slice(0, 1)))
    const { container } = await mount(vi.fn(), 'c_qrstuvwx')
    await waitFor(() => container.querySelectorAll('.fh-row').length === 3)
    expect(mockHistory).not.toHaveBeenCalledWith('src/lib/api.ts', false)
    expect(mockHistory).toHaveBeenCalledWith('src/lib/api.ts', true)
    expect(container.querySelector('.fh-pinned')?.getAttribute('data-idx')).toBe('2')
  })

  it('initialPin not in history → A falls back to the newest row', async () => {
    const { container } = await mount(vi.fn(), 'c_nope')
    await waitFor(() => container.querySelectorAll('.fh-row').length === 3)
    expect(container.querySelector('.fh-pinned')?.getAttribute('data-idx')).toBe('0')
  })

  // List swap (mutable → "Load full history") must carry cursor B and pin A
  // across by identity, not leave raw indices pointing at whatever row lands.
  it('Load full history keeps cursor AND pin on their (different) commits', async () => {
    // Mutable tier: middle + oldest (A latches to middle at row 0). Move B to
    // oldest, then Load full → A must follow "middle" (→1), B "oldest" (→2);
    // a regression deriving one from the other, or leaving raw indices, fails.
    mockHistory.mockImplementation((_p: string, full: boolean) =>
      Promise.resolve(full ? REVS : [REVS[1], REVS[2]]))
    const { component, container } = await mount()
    await waitFor(() => container.querySelectorAll('.fh-row').length === 2)
    kd(component, 'j'); await tick()
    expect(container.querySelector('.fh-cursor')?.getAttribute('data-idx')).toBe('1')
    expect(container.querySelector('.fh-pinned')?.getAttribute('data-idx')).toBe('0')
    await fireEvent.click(container.querySelector('.fh-load-full')!)
    await waitFor(() => container.querySelectorAll('.fh-row').length === 3)
    expect(container.querySelector('.fh-pinned')?.getAttribute('data-idx')).toBe('1')
    expect(container.querySelector('.fh-cursor')?.getAttribute('data-idx')).toBe('2')
    // Space after the swap re-pins A to B's commit (identity, not stale index).
    kd(component, 'k'); kd(component, 'k'); kd(component, ' '); await tick()
    expect(container.querySelector('.fh-pinned')?.getAttribute('data-idx')).toBe('0')
  })

  // #38: split/unified toggle is panel-local (seeded from config, never
  // written back — DiffPanel underneath guards editor buffers on that key).
  it("'|' and the header button toggle split view for the rendered diff", async () => {
    mockDiffRange.mockResolvedValue({ diff: [
      'diff --git a/src/lib/api.ts b/src/lib/api.ts',
      '--- a/src/lib/api.ts', '+++ b/src/lib/api.ts',
      '@@ -1,1 +1,1 @@', '-old', '+new', '',
    ].join('\n') })
    vi.useFakeTimers()
    let r: Awaited<ReturnType<typeof mount>>
    try {
      r = await mount()
      kd(r.component, 'j'); await tick()
      vi.advanceTimersByTime(50)
    } finally { vi.useRealTimers() }
    const { component, container } = r
    await waitFor(() => container.querySelector('.diff-file') !== null)
    expect(container.querySelector('.split-view')).toBeNull()
    expect(kd(component, '|')).toBe(true)
    await tick()
    expect(container.querySelector('.split-view')).not.toBeNull()
    await fireEvent.click(container.querySelector('.panel-actions .btn')!)
    expect(container.querySelector('.split-view')).toBeNull()
  })

  it('handleKeydown returns true only for consumed keys', async () => {
    const { component } = await mount()
    expect(kd(component, 'j')).toBe(true)
    expect(kd(component, 'ArrowUp')).toBe(true)
    expect(kd(component, ' ')).toBe(true)
    expect(kd(component, 'x')).toBe(false)
  })
})
