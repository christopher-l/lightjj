import { describe, it, expect } from 'vitest'
import { highlightLines } from './highlighter'
import { ensureLegacyParsers, LANGUAGES, PARSERS } from './languages'

// Registry-level behavior of individual LANGUAGES entries, observed through
// highlightLines (the only consumer that matters for the diff view). The
// symbol-hover (GET /api/symbol) is reachable for a language only if its
// identifiers come out as tok-variableName/typeName/propertyName/className
// spans — highlighter.ts stamps data-sym on exactly those — so these tests pin
// identifier classing, not just "something got colored".

const sym = (name: string, tok: string) => new RegExp(`class="tok-${tok}[^"]*" data-sym>${name}<`)

describe('svelte: nested TS/CSS parsing', () => {
  it('script bodies parse as TypeScript — identifiers are classed and hoverable', () => {
    const out = highlightLines([
      '<script lang="ts">',
      '  const foo: number = 1',
      '  function bar() { return foo }',
      '</script>',
    ], 'svelte')
    expect(out).toHaveLength(4)
    expect(out[0]).toMatch(/tok-typeName[^>]*>script/) // tag shell still HTML
    expect(out[1]).toContain('tok-keyword')             // const — was plain text before nesting
    expect(out[1]).toMatch(sym('foo', 'variableName'))
    expect(out[1]).toMatch(/tok-typeName[^>]*>number/)
    expect(out[2]).toMatch(sym('bar', 'variableName'))
  })

  it('plain <script> (no lang attr) also nests', () => {
    const out = highlightLines(['<script>', 'let x = 1', '</script>'], 'svelte')
    expect(out[1]).toContain('tok-keyword')
    expect(out[1]).toMatch(sym('x', 'variableName'))
  })

  it('style bodies parse as CSS', () => {
    const out = highlightLines(['<style>.card { color: red }</style>'], 'svelte')
    expect(out[0]).toMatch(/tok-className[^>]*>card/)
    expect(out[0]).toMatch(/tok-propertyName[^>]*>color/)
  })

  it('selfClosing dialect: <Component /> parses as a tag, markup after it still highlights', () => {
    const out = highlightLines(['<Foo bind:x={y} />', '<div class="a">{z}</div>'], 'svelte')
    expect(out[0]).toMatch(/tok-typeName[^>]*>Foo/)
    expect(out[1]).toMatch(/tok-typeName[^>]*>div/)
    expect(out[1]).toContain('tok-string') // "a"
    expect(out[1]).toContain('{z}')        // interpolation stays plain
  })

  it('known limitation: a hunk inside <script> without the opener in view stays plain', () => {
    // Per-hunk highlighting feeds only the hunk's lines; with no <script> tag
    // the HTML grammar sees text. Pinned so a future fix (or regression of the
    // opener case above) is visible — flip this when hunk highlighting learns
    // the enclosing block.
    const out = highlightLines(['  const foo = 1'], 'svelte')
    expect(out[0]).not.toContain('tok-keyword')
  })

  it('typescript entry shares the same TS-dialect parser instance', () => {
    expect(PARSERS.typescript).toBe(LANGUAGES.typescript.parser)
    const out = highlightLines(['const x: string = "hi"'], 'typescript')
    expect(out[0]).toMatch(/tok-typeName[^>]*>string/)
  })
})

describe('zig: identifiers are classed (symbol-hover reachability)', () => {
  it('TitleCase → typeName, other identifiers → variableName, both data-sym', async () => {
    await ensureLegacyParsers()
    const out = highlightLines([
      'const Point = struct {',
      'pub fn init(alloc: Allocator) Self {',
      '    self.count += 1;',
      'const std = @import("std");',
    ], 'zig')
    expect(out).toHaveLength(4)
    expect(out[0]).toMatch(sym('Point', 'typeName'))
    expect(out[0]).toContain('tok-keyword') // const / struct unchanged
    expect(out[0]).not.toMatch(/data-sym>(const|struct)</)
    expect(out[1]).toMatch(sym('init', 'variableName'))
    expect(out[1]).toMatch(sym('alloc', 'variableName'))
    expect(out[1]).toMatch(sym('Allocator', 'typeName'))
    expect(out[1]).toMatch(sym('Self', 'typeName'))
    expect(out[2]).toMatch(sym('self', 'variableName'))
    expect(out[2]).toMatch(sym('count', 'variableName'))
    expect(out[3]).toMatch(sym('std', 'variableName'))
    expect(out[3]).toContain('tok-string')
  })

  it('keywords, primitive types, atoms and @builtins keep their classes', async () => {
    await ensureLegacyParsers()
    const out = highlightLines(['var n: u32 = if (true) @sizeOf(u8) else undefined;'], 'zig')
    expect(out[0]).toMatch(/tok-keyword[^>]*>var/)
    expect(out[0]).toMatch(/tok-keyword[^>]*>if/)
    expect(out[0]).toMatch(/tok-typeName[^>]*>u32/)
    expect(out[0]).toMatch(/tok-atom[^>]*>true/)
    expect(out[0]).toMatch(/tok-atom[^>]*>undefined/)
    expect(out[0]).toMatch(/tok-variableName[^>]*>@sizeOf/)
    expect(out[0]).toMatch(sym('n', 'variableName'))
  })
})
