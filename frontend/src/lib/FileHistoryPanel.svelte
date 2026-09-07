<script lang="ts">
  import { untrack } from 'svelte'
  import { api, type LogEntry } from './api'
  import { config } from './config.svelte'
  import { createLoader, DIFF_LOAD_DEBOUNCE_MS } from './loader.svelte'
  import { parseDiffContent } from './diff-parser'
  import { relativeTime, firstLine } from './time-format'
  import DiffFileView from './DiffFileView.svelte'
  import FileHistoryRail from './FileHistoryRail.svelte'
  import SplitToggle from './SplitToggle.svelte'

  interface Props {
    path: string
    /** commit_id to pre-pin as A. When set the rail skips the mutable tier
     *  and loads full history directly (the target is rarely WIP); if it's
     *  not in that list either, A falls back to the newest row. */
    initialPin?: string | null
    onclose: () => void
  }

  let { path, initialPin, onclose }: Props = $props()

  // ── Two-cursor state ─────────────────────────────────────────────────────
  // cursorB moves with j/k (rail owns it, identity-stable across list swaps);
  // pin A is keyed by commit_id and its index DERIVED, so whichever history
  // tier lands (and any later refetch) can't re-bind A to a different commit
  // (CLAUDE.md: key selection by identity). initialPin seeds it; when set the
  // rail gets startFull — the target is rarely mutable WIP. Not found (or no
  // pin) → row 0, the newest. Space re-pins to the cursor's commit.
  let revisions: LogEntry[] = $state([])
  let cursorB = $state(0)
  // untrack: seed only — the prop never changes mid-lifetime ({#key} remounts
  // per open); silences state_referenced_locally like App's initialState.
  let pinnedId: string | null = $state(untrack(() => initialPin ?? null))
  const pinIdx = (id: string | null) => id ? revisions.findIndex(r => r.commit.commit_id === id) : -1
  let pinnedA = $derived(Math.max(0, pinIdx(pinnedId)))
  // Latch an unresolved pin (none given, or initialPin absent from the list)
  // onto the row it's displayed as — so a later list swap ("Load full
  // history") carries A with that commit instead of re-binding to the new row 0.
  $effect(() => {
    if (revisions.length > 0 && pinIdx(pinnedId) < 0) pinnedId = revisions[0].commit.commit_id
  })
  // bug_015: per-file collapse — diffRange can return multiple entries on renames.
  let collapsed = $state(new Set<string>())
  let railRef: FileHistoryRail | undefined = $state()
  // Split/unified is panel-LOCAL, seeded from the user's global choice. Never
  // write config.splitView from here: DiffPanel is mounted underneath and its
  // toggleSplitView() confirm guards an open FileEditor buffer — a config
  // write would flip it via the $bindable and silently discard those edits.
  let splitView = $state(untrack(() => config.splitView))

  let revA = $derived(revisions[pinnedA])
  let revB = $derived(revisions[cursorB])
  let sameRev = $derived(pinnedA === cursorB)

  // ── Diff loader ──────────────────────────────────────────────────────────
  const diff = createLoader(
    async (from: string, to: string) => {
      const r = await api.diffRange(from, to, [path])
      return parseDiffContent(r.diff)
    },
    [] as ReturnType<typeof parseDiffContent>,
  )

  // bug_001: diffRange(from, to) shows "what changed going from→to". With
  // A=newest (pinned) and B=cursor moving DOWN to older commits, the intuitive
  // read is "what did A add relative to B?" → from=B, to=A. Green = additions.
  // bug_027: 50ms debounce so rapid j/k doesn't fire N requests.
  let debounce: ReturnType<typeof setTimeout> | undefined
  $effect(() => {
    if (sameRev || !revA || !revB) { diff.reset(); return }
    const a = revA.commit.commit_id
    const b = revB.commit.commit_id
    clearTimeout(debounce)
    debounce = setTimeout(() => diff.load(b, a), DIFF_LOAD_DEBOUNCE_MS)
    return () => clearTimeout(debounce)
  })

  /** Exported for App delegation. Rail handles j/k; we handle Space/Escape. */
  export function handleKeydown(e: KeyboardEvent): boolean {
    if (railRef?.handleKeydown(e)) return true
    switch (e.key) {
      case ' ':
        pinnedId = revisions[cursorB]?.commit.commit_id ?? null
        return true
      case '|':
        splitView = !splitView
        return true
      case 'Escape':
        onclose()
        return true
    }
    return false
  }

  // Stable empty maps for DiffFileView props (same pattern as EvologPanel).
  const EMPTY_HL = new Map<string, string>()
  const EMPTY_WD = new Map<string, Map<number, import('./word-diff').WordSpan[]>>()
</script>

<div class="fh-root">
  <div class="fh-header">
    <span class="fh-title">File history: <code>{path}</code></span>
    <span class="panel-actions">
      <SplitToggle split={splitView} onclick={() => splitView = !splitView} hint="|" />
      <button class="close-btn" onclick={onclose} title="Close (Escape)">✕</button>
    </span>
  </div>

  <div class="fh-body">
    <FileHistoryRail
      bind:this={railRef}
      {path}
      pinnedIndex={pinnedA}
      startFull={!!initialPin}
      bind:revisions
      bind:selectedIndex={cursorB}
    />

    <!-- ── Right: A/B cards + diff ─────────────────────────────────────── -->
    <div class="fh-diff-side">
      {#if revA && revB}
        <div class="fh-cards">
          <div class="fh-card fh-card-a">
            <div class="fh-card-label">A <span class="fh-card-hint">(pinned — Space to re-pin)</span></div>
            <code class="fh-card-id">{revA.commit.change_id.slice(0, 8)}</code>
            <span class="fh-card-desc truncate">{firstLine(revA.description) || '(no description)'}</span>
            <span class="fh-card-age">{relativeTime(revA.commit.timestamp)}</span>
          </div>
          <span class="fh-swap">⇄</span>
          <div class="fh-card fh-card-b">
            <div class="fh-card-label">B <span class="fh-card-hint">(cursor — j/k)</span></div>
            <code class="fh-card-id">{revB.commit.change_id.slice(0, 8)}</code>
            <span class="fh-card-desc truncate">{firstLine(revB.description) || '(no description)'}</span>
            <span class="fh-card-age">{relativeTime(revB.commit.timestamp)}</span>
          </div>
        </div>
      {/if}

      <div class="fh-diff-scroll">
        {#if sameRev}
          <div class="fh-empty-state">Same revision — press <kbd>j</kbd>/<kbd>k</kbd> to compare</div>
        {:else if diff.loading}
          <div class="fh-empty-state">Loading diff…</div>
        {:else if diff.error}
          <div class="fh-empty-state fh-error">{diff.error}</div>
        {:else if diff.value.length === 0}
          <div class="fh-empty-state">No changes between A and B for this file.</div>
        {:else}
          {#each diff.value as file (file.filePath)}
            <DiffFileView
              {file}
              fileStats={undefined}
              isCollapsed={collapsed.has(file.filePath)}
              isExpanded={false}
              {splitView}
              highlightedLines={EMPTY_HL}
              wordDiffs={EMPTY_WD}
              ontoggle={() => {
                const next = new Set(collapsed)
                next.has(file.filePath) ? next.delete(file.filePath) : next.add(file.filePath)
                collapsed = next
              }}
            />
          {/each}
        {/if}
      </div>
    </div>
  </div>
</div>

<style>
  .fh-root {
    display: flex;
    flex-direction: column;
    height: 100%;
    width: 100%;  /* bug_020: parent overlay is display:flex → child needs explicit fill */
    background: var(--base);
  }
  .fh-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 12px;
    border-bottom: 1px solid var(--surface0);
    font-size: var(--fs-md);
  }
  .fh-title code {
    font-family: var(--font-mono);
    color: var(--text);
  }
  .fh-body {
    display: flex;
    flex: 1;
    min-height: 0;
  }

  /* ── Right side ── */
  .fh-diff-side {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .fh-cards {
    display: flex;
    align-items: stretch;
    gap: 8px;
    padding: 8px 12px;
    border-bottom: 1px solid var(--surface0);
  }
  .fh-card {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 6px 8px;
    background: var(--surface0);
    border-radius: 4px;
    border-left: 3px solid transparent;
    font-size: var(--fs-sm);
  }
  .fh-card-a { border-left-color: var(--amber); }
  .fh-card-label {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    font-weight: bold;
    color: var(--amber);
  }
  .fh-card-hint {
    font-weight: normal;
    color: var(--subtext0);
  }
  .fh-card-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--subtext1);
  }
  .fh-card-age {
    font-size: var(--fs-2xs);
    color: var(--subtext0);
  }
  .fh-swap {
    align-self: center;
    color: var(--subtext0);
    font-size: var(--fs-lg);
  }
  .fh-diff-scroll {
    flex: 1;
    overflow-y: auto;
  }
  .fh-empty-state {
    padding: 40px;
    text-align: center;
    color: var(--subtext0);
    font-size: var(--fs-md);
  }
  .fh-empty-state kbd {
    padding: 1px 4px;
    border: 1px solid var(--surface1);
    border-radius: 3px;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .fh-error { color: var(--red); }
</style>
