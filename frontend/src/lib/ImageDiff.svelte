<script lang="ts">
  // Image diff — the body DiffFileView renders for textless image entries
  // (binary png/jpg…, or a hunk-less pure rename) instead of "Binary file —
  // not diffable". Pure presentation: the parent resolves the two
  // /api/file-raw URLs (after = the revision, before = its parent) and re-keys
  // this component when either changes, so all per-image state here
  // (dimensions, load errors, slider position) is fresh per revision without a
  // reset effect.
  //
  // beforeSrc undefined = nothing to compare against (Added; merge commit —
  // the parent says why via baseNote; multi-root range); afterSrc undefined =
  // Deleted. A src that still 404s (multi-rev revset fallback, rename source
  // the parser didn't see) drops that side via <img onerror> and leaves a
  // neutral note rather than a broken-image glyph.

  interface Props {
    path: string
    beforeSrc?: string
    afterSrc?: string
    /** Up-front reason there is no before side (e.g. merge commit), from the
     *  parent. Shown only when beforeSrc is absent and the change isn't an Add
     *  (an Added image has no previous version by definition — no note). */
    baseNote?: string
    /** FileChange.type ('A'|'D'|'M'|'R'…) — only picks the single-pane label
     *  (Added/Deleted vs After/Before); which sides exist comes from the srcs. */
    changeType?: string
    /** Side-by-side panes (split diff view) vs swipe slider (unified). */
    split: boolean
  }

  let { path, beforeSrc, afterSrc, baseNote, changeType, split }: Props = $props()

  type Side = 'before' | 'after'
  type Dims = { w: number; h: number }
  // null = not loaded yet (jsdom never fires onload — tests see this state).
  let dims = $state<Record<Side, Dims | 'error' | null>>({ before: null, after: null })
  let pct = $state(50) // divider position: left pct% shows Before, rest After

  function loaded(side: Side, e: Event) {
    const img = e.currentTarget as HTMLImageElement
    dims[side] = { w: img.naturalWidth, h: img.naturalHeight }
  }
  const failed = (side: Side) => () => { dims[side] = 'error' }
  // Usable dimensions only: an intrinsic-size-less SVG (mis-detected as
  // binary; Firefox reports naturalWidth 0) loads fine but 0×0 would give
  // fw=0 → invisible + a "0×0" caption. Treat it as "loaded, size unknown":
  // still shown (1:1 fill / natural layout), no dimensions text.
  const ok = (side: Side): Dims | null => {
    const d = dims[side]
    return d && d !== 'error' && d.w > 0 && d.h > 0 ? d : null
  }
  const dimText = (side: Side) => { const d = ok(side); return d ? `${d.w}×${d.h}` : '' }

  let hasBefore = $derived(!!beforeSrc && dims.before !== 'error')
  let hasAfter = $derived(!!afterSrc && dims.after !== 'error')

  // Slider stage: both images must render at ONE common scale or the swipe
  // lies. Stage = the larger natural box (mw×mh), shrunk to fit the panel
  // width and 60vh; each image is sized as its fraction of mw, anchored
  // top-left. Until onload reports dimensions, fall back to a 1:1 fill.
  let mw = $derived(Math.max(ok('before')?.w ?? 0, ok('after')?.w ?? 0) || 300)
  let mh = $derived(Math.max(ok('before')?.h ?? 0, ok('after')?.h ?? 0) || 150)
  const fw = (side: Side) => { const d = ok(side); return d ? d.w / mw : 1 }

  // Click/drag anywhere on the stage moves the divider (the range input below
  // is the keyboard path). Pointer capture keeps the drag alive off-element.
  // Primary button only — a right/middle click must not seek + capture.
  function seek(e: PointerEvent) {
    const el = e.currentTarget as HTMLElement
    if (e.type === 'pointerdown') {
      if (e.button !== 0) return
      el.setPointerCapture(e.pointerId)
    } else if (!el.hasPointerCapture(e.pointerId)) return
    const r = el.getBoundingClientRect()
    if (r.width > 0) pct = Math.max(0, Math.min(100, ((e.clientX - r.left) / r.width) * 100))
  }
  // A mouse click/drag on the range leaves it focused → App's isInInput()
  // gate would swallow j/k/[/]/Esc/1-5 until the user clicks elsewhere. Blur
  // on pointer release so only a Tab-focused keyboard user keeps focus (and
  // with it the arrow-key control they came for).
  const blurOnPointer = (e: PointerEvent) => (e.currentTarget as HTMLElement).blur()
</script>

{#snippet caption(side: Side, label: string)}
  <span class="img-label img-label-{side}">{label}</span>
  {#if dimText(side)}<span class="img-dims">{dimText(side)}</span>{/if}
{/snippet}

{#snippet pane(side: Side, label: string, src: string)}
  <figure class="img-pane img-pane-{side}">
    <figcaption class="img-cap">{@render caption(side, label)}</figcaption>
    <div class="img-frame">
      <img class="img-fit" {src} alt="{label}: {path}" loading="lazy" onload={(e) => loaded(side, e)} onerror={failed(side)} />
    </div>
  </figure>
{/snippet}

<div class="image-diff" class:image-diff-split={split && hasBefore && hasAfter}>
  {#if hasBefore && hasAfter}
    {#if split}
      {@render pane('before', 'Before', beforeSrc!)}
      {@render pane('after', 'After', afterSrc!)}
    {:else}
      <figure class="img-pane img-pane-compare" style:--iw={mw} style:--ih={mh}>
        <figcaption class="img-cap img-cap-compare">
          <span class="img-cap-side">{@render caption('before', 'Before')}</span>
          <span class="img-cap-side">{@render caption('after', 'After')}</span>
        </figcaption>
        <div class="img-frame">
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div class="ic-stage" onpointerdown={seek} onpointermove={seek}>
            <!-- BOTH sides clipped to their half: stacking After over an
                 unclipped Before would let Before bleed through After's
                 transparent pixels instead of showing the checkerboard. -->
            <div class="ic-clip" style:clip-path="inset(0 {100 - pct}% 0 0)">
              <img class="ic-img" style:--fw={fw('before')} src={beforeSrc} alt="Before: {path}" loading="lazy" draggable="false" onload={(e) => loaded('before', e)} onerror={failed('before')} />
            </div>
            <div class="ic-clip" style:clip-path="inset(0 0 0 {pct}%)">
              <img class="ic-img" style:--fw={fw('after')} src={afterSrc} alt="After: {path}" loading="lazy" draggable="false" onload={(e) => loaded('after', e)} onerror={failed('after')} />
            </div>
            <div class="ic-divider" style:left="{pct}%" aria-hidden="true"></div>
          </div>
        </div>
        <input class="ic-range" type="range" min="0" max="100" step="1" bind:value={pct} onpointerup={blurOnPointer} onpointercancel={blurOnPointer} aria-label="Image comparison divider — left of it shows Before, right shows After" />
      </figure>
    {/if}
  {:else if hasAfter}
    {@render pane('after', changeType === 'A' ? 'Added' : 'After', afterSrc!)}
  {:else if hasBefore}
    {@render pane('before', changeType === 'D' ? 'Deleted' : 'Before', beforeSrc!)}
  {/if}
  {#if !beforeSrc && baseNote && changeType !== 'A'}
    <div class="img-note placeholder-text">({baseNote})</div>
  {:else if beforeSrc && dims.before === 'error'}
    <div class="img-note placeholder-text">(previous version unavailable)</div>
  {/if}
  {#if afterSrc && dims.after === 'error'}
    <div class="img-note placeholder-text">(image unavailable at this revision)</div>
  {/if}
</div>

<style>
  .image-diff {
    padding: 10px 12px 12px;
    display: grid;
    gap: 12px;
  }
  .image-diff-split {
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  }

  .img-pane {
    margin: 0;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .img-cap {
    display: flex;
    align-items: baseline;
    gap: 6px;
    font-size: var(--fs-xs);
  }
  .img-cap-compare {
    justify-content: space-between;
  }
  .img-cap-side {
    display: inline-flex;
    align-items: baseline;
    gap: 6px;
  }
  .img-label {
    font-weight: 600;
    padding: 0 5px;
    border-radius: 3px;
    line-height: 16px;
  }
  .img-label-before { background: var(--badge-delete-bg); color: var(--red); }
  .img-label-after { background: var(--badge-add-bg); color: var(--green); }
  .img-dims {
    font-family: var(--font-mono);
    color: var(--text-faint);
  }

  /* Checkerboard so transparent PNGs read as transparent, not as --base. */
  .img-frame {
    align-self: flex-start;
    max-width: 100%;
    line-height: 0;
    border: 1px solid var(--surface1);
    background:
      repeating-conic-gradient(var(--surface1) 0% 25%, var(--base) 0% 50%) 0 0 / 16px 16px;
  }
  .img-fit {
    display: block;
    max-width: 100%;
    max-height: 60vh;
    object-fit: contain;
  }

  /* Swipe compare: caption, stage and range share ONE width — the larger
     image's natural width, shrunk to the pane and to 60vh of height — so the
     Before/After captions sit over their halves and the range thumb tracks
     the divider. --iw/--ih come from the script (natural px, unitless). */
  .img-pane-compare > * {
    width: min(100%, calc(var(--iw) * 1px), calc(60vh * var(--iw) / var(--ih)));
  }
  .img-pane-compare .img-frame {
    align-self: auto;
  }
  .ic-stage {
    position: relative;
    aspect-ratio: var(--iw) / var(--ih);
    cursor: ew-resize;
    user-select: none;
    touch-action: none;
    overflow: hidden;
  }
  .ic-img {
    position: absolute;
    top: 0;
    left: 0;
    width: calc(100% * var(--fw, 1));
    height: auto;
    pointer-events: none;
  }
  .ic-clip {
    position: absolute;
    inset: 0;
  }
  .ic-divider {
    position: absolute;
    top: 0;
    bottom: 0;
    width: 2px;
    margin-left: -1px;
    background: var(--amber);
    box-shadow: 0 0 0 1px color-mix(in srgb, var(--base) 60%, transparent);
    pointer-events: none;
  }
  .ic-range {
    accent-color: var(--amber);
    margin: 0;
  }

  .img-note {
    font-size: var(--fs-xs);
  }
</style>
