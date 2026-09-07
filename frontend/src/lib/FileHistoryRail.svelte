<script lang="ts">
  import { untrack } from 'svelte'
  import { api, type LogEntry } from './api'
  import { createLoader } from './loader.svelte'
  import { relativeTime, firstLine } from './time-format'
  import { scrollIdxIntoView } from './scroll-into-view'

  interface Props {
    path: string
    /** Shows 'A' badge at this index. Undefined = single-cursor mode (inline picker). */
    pinnedIndex?: number
    /** Skip the mutable-scoped tier and loadFull on mount. Set when the caller
     *  needs a specific commit that's unlikely to be in mutable WIP (initialPin). */
    startFull?: boolean
    /** Bindable output — the loaded revision list. */
    revisions?: LogEntry[]
    /** Bindable output — cursor position. */
    selectedIndex?: number
  }

  let {
    path,
    pinnedIndex,
    startFull = false,
    revisions = $bindable([]),
    selectedIndex = $bindable(0),
  }: Props = $props()

  // ── Two-tier history loading ────────────────────────────────────────────
  // mutable-only is instant but shows only WIP. full=true drops the scope —
  // fast once jj's changed-path index is built (jj#7250). Index build is a
  // separate streaming call (no timeout) so first-build on large repos
  // doesn't hit the 30s read ceiling.
  let full = $state(false)
  let indexProgress = $state('')
  const history = createLoader(
    ([p, f]: [string, boolean]) => api.fileHistory(p, f),
    [] as LogEntry[],
  )
  // startFull: skip the mutable tier ENTIRELY — don't race it against
  // loadFull(). FileHistoryPanel latches an unresolved pin onto the FIRST
  // non-empty list's row 0; if the mutable tier landed first, an initialPin
  // outside WIP would be discarded before the full list ever arrived.
  $effect(() => { if (startFull && !full) return; history.load([path, full]) })
  // List swap (mutable → full, or a refetch) re-binds a raw index to whatever
  // row lands there. Carry the cursor across by commit_id (CLAUDE.md: key
  // selection by identity, derive the index); untrack so this effect depends
  // only on history.value, not on the outputs it writes.
  $effect(() => {
    const next = history.value
    const prevId = untrack(() => revisions[selectedIndex]?.commit.commit_id)
    revisions = next
    selectedIndex = Math.max(0, next.findIndex(r => r.commit.commit_id === prevId))
    // Carried row may sit far down a long full list — keep it on screen.
    // (Render effects flush before this user effect, so the row exists.)
    if (next.length > 0) scrollTo(untrack(() => selectedIndex))
  })
  $effect(() => { if (startFull) loadFull() })

  let sparse = $derived(!full && !history.loading && history.value.length < 5)

  export async function loadFull() {
    if (indexProgress || full) return  // re-entry guard
    const started = Date.now()
    const tick = () => {
      indexProgress = `Building path index… ${Math.floor((Date.now() - started) / 1000)}s`
    }
    tick()
    const timer = setInterval(tick, 1000)
    try {
      await api.indexPaths(line => { indexProgress = line })
    } catch (e) {
      console.warn('index-changed-paths failed:', e)
    } finally {
      clearInterval(timer)
      indexProgress = ''
    }
    // Unconditional — files() works without the index, just slowly.
    full = true
  }

  // ── Navigation ──────────────────────────────────────────────────────────
  let hoveredIdx = $state(-1)
  let listEl: HTMLElement | undefined = $state()

  function scrollTo(i: number) {
    scrollIdxIntoView(listEl, i)
  }

  export function moveCursor(i: number) {
    if (i < 0 || i >= history.value.length) return
    selectedIndex = i
    scrollTo(i)
  }

  /** j/k navigation. Returns true if key consumed. Parent handles Space/Escape. */
  export function handleKeydown(e: KeyboardEvent): boolean {
    switch (e.key) {
      case 'j': case 'ArrowDown':
        moveCursor(selectedIndex + 1)
        return true
      case 'k': case 'ArrowUp':
        moveCursor(selectedIndex - 1)
        return true
    }
    return false
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions a11y_mouse_events_have_key_events -->
<div class="fh-rail" bind:this={listEl}
  onmousemove={e => {
    const t = (e.target as Element).closest('[data-idx]')
    hoveredIdx = t ? Number(t.getAttribute('data-idx')) : -1
  }}
  onmouseleave={() => hoveredIdx = -1}
>
  {#if indexProgress}
    <div class="fh-empty">
      {indexProgress}
      <br><small>First run per repo builds jj's changed-path index (one-time). Subsequent loads are instant.</small>
      <br><small>Large repo? Run <code>jj debug index-changed-paths</code> in a terminal for a live progress bar.</small>
    </div>
  {:else if history.loading && history.value.length === 0}
    <div class="fh-empty">Loading…</div>
  {:else if history.error}
    <div class="fh-empty fh-error">{history.error}</div>
  {:else if history.value.length === 0}
    <div class="fh-empty">
      {full ? 'No revisions touch this file.' : 'No mutable revisions touch this file.'}
      {#if !full}<br><button class="fh-load-full" onclick={loadFull}>Load full history</button>{/if}
    </div>
  {:else}
    {#each history.value as rev, i (rev.commit.commit_id)}
      {@const c = rev.commit}
      <button
        class="fh-row"
        class:fh-cursor={i === selectedIndex}
        class:fh-pinned={i === pinnedIndex}
        class:fh-hovered={i === hoveredIdx}
        class:fh-immutable={c.immutable}
        data-idx={i}
        onclick={() => moveCursor(i)}
      >
        <span class="fh-badge">{i === pinnedIndex ? 'A' : i === selectedIndex ? (pinnedIndex !== undefined ? 'B' : '●') : ''}</span>
        <code class="fh-id">{c.change_id.slice(0, c.change_prefix)}<span class="fh-id-rest">{c.change_id.slice(c.change_prefix, 8)}</span></code>
        <span class="fh-desc">{firstLine(rev.description) || '(no description)'}</span>
        <span class="fh-age">{relativeTime(c.timestamp)}</span>
      </button>
    {/each}
    {#if sparse}
      <div class="fh-sparse">
        <small>Only {history.value.length} mutable commit{history.value.length === 1 ? '' : 's'} — showing your WIP only.</small>
        <button class="fh-load-full" onclick={loadFull}>Load full history</button>
      </div>
    {/if}
  {/if}
</div>

<style>
  .fh-rail {
    min-width: 240px;
    max-width: 340px;
    overflow-y: auto;
    background: var(--mantle);
    border-right: 1px solid var(--surface0);
    user-select: none;
  }
  .fh-row {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    height: 18px;
    padding: 0 8px;
    border: none;
    border-left: 2px solid transparent;
    background: transparent;
    color: var(--text);
    font-family: inherit;
    font-size: var(--fs-sm);
    text-align: left;
    cursor: pointer;
    overflow: hidden;
  }
  .fh-immutable { opacity: 0.6; }
  .fh-hovered { background: var(--surface0); }
  .fh-cursor {
    background: color-mix(in srgb, var(--amber) 12%, transparent);
    border-left-color: var(--amber);
  }
  .fh-cursor.fh-hovered {
    background: color-mix(in srgb, var(--amber) 18%, transparent);
  }
  .fh-pinned:not(.fh-cursor) {
    border-left-color: var(--amber);
  }
  .fh-badge {
    width: 10px;
    font-family: var(--font-mono);
    font-size: var(--fs-2xs);
    font-weight: bold;
    color: var(--amber);
    text-align: center;
  }
  .fh-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--amber);
  }
  .fh-id-rest { color: var(--subtext0); }
  .fh-desc {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .fh-age {
    font-size: var(--fs-2xs);
    color: var(--subtext0);
    font-family: var(--font-mono);
  }
  .fh-sparse {
    padding: 10px;
    text-align: center;
    color: var(--subtext0);
    border-top: 1px solid var(--surface0);
  }
  .fh-sparse small { display: block; margin-bottom: 6px; font-size: var(--fs-xs); }
  .fh-load-full {
    padding: 4px 10px;
    border: 1px solid var(--surface1);
    border-radius: 4px;
    background: var(--surface0);
    color: var(--text);
    font-family: inherit;
    font-size: var(--fs-sm);
    cursor: pointer;
  }
  .fh-load-full:hover { background: var(--surface1); }
  .fh-empty {
    padding: 16px;
    text-align: center;
    color: var(--subtext0);
    font-size: var(--fs-sm);
  }
  .fh-error { color: var(--red); }
</style>
