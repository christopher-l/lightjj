<script lang="ts">
  // The ◫/≡ split/unified toggle button — one owner for glyphs + a11y strings
  // across DiffPanel, FileHistoryPanel, FileComparePicker. Pure presentational:
  // the parent owns the boolean (DiffPanel routes through its editor-buffer
  // confirm; the overlays keep panel-local state).
  interface Props {
    split: boolean
    onclick: () => void
    /** Keyboard hint appended to the tooltip + aria-keyshortcuts, e.g. '|'. */
    hint?: string
  }
  let { split, onclick, hint }: Props = $props()
  let suffix = $derived(hint ? ` (${hint})` : '')
</script>

<button
  class="btn btn-sm"
  {onclick}
  title={split ? `Split view — click for unified${suffix}` : `Unified view — click for split${suffix}`}
  aria-label={split ? 'Switch to unified view' : 'Switch to split view'}
  aria-pressed={split}
  aria-keyshortcuts={hint}
>{split ? '◫' : '≡'}</button>
