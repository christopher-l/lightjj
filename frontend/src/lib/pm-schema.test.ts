import { describe, it, expect } from 'vitest'
import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { DOMSerializer } from 'prosemirror-model'
import { parseMarkdown, serializeMarkdown, docSchema } from './pm-schema'

const rt = (md: string) => serializeMarkdown(parseMarkdown(md))

function renderToDOM(md: string): HTMLElement | DocumentFragment {
  const doc = parseMarkdown(md)
  return DOMSerializer.fromSchema(docSchema).serializeFragment(doc.content)
}

describe('pm-schema XSS guards', () => {
  it.each([
    ['javascript:alert(1)', '#'],
    ['JAVASCRIPT:alert(1)', '#'],
    ['vbscript:msgbox(1)', '#'],
    ['data:text/html,<script>alert(1)</script>', '#'],
    ['https://example.com', 'https://example.com'],
    ['http://example.com', 'http://example.com'],
    ['mailto:a@b.co', 'mailto:a@b.co'],
    ['#anchor', '#anchor'],
    ['./relative.md', './relative.md'],
    ['/abs/path', '/abs/path'],
  ])('link scheme gate: %s → %s', (href, expected) => {
    const frag = renderToDOM(`[x](${href})`)
    const a = frag.querySelector('a')!
    expect(a.getAttribute('href')).toBe(expected)
    expect(a.getAttribute('rel')).toBe('noopener noreferrer nofollow')
  })

  it('unsafe href survives serialize round-trip (only DOM is neutered)', () => {
    const md = '[x](javascript:alert(1))\n'
    expect(rt(md)).toBe(md)
  })

  it('passthrough renders raw as TEXT not HTML', () => {
    const frag = renderToDOM('<script>alert(1)</script>')
    expect(frag.querySelector('script')).toBeNull()
    expect(frag.querySelector('.pm-passthrough')?.textContent).toContain('<script>')
  })

  it.each([
    ['javascript:alert(1)', ''],
    ['data:text/html,x', ''],
    ['https://example.com/x.png', 'https://example.com/x.png'],
    ['./local.png', './local.png'],
  ])('image src scheme gate: %s → %s', (src, expected) => {
    const frag = renderToDOM(`![alt](${src})`)
    expect(frag.querySelector('img')?.getAttribute('src')).toBe(expected)
  })
})

describe('pm-schema GFM extensions', () => {
  it('table round-trips', () => {
    const md = '| a | b |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |\n'
    expect(rt(md)).toBe(md)
  })

  it('table with alignment', () => {
    const md = '| l | c | r |\n| :--- | :---: | ---: |\n| 1 | 2 | 3 |\n'
    expect(rt(md)).toBe(md)
  })

  it('table cell with pipe escaped', () => {
    const md = '| a |\n| --- |\n| x\\|y |\n'
    expect(rt(md)).toBe(md)
  })

  it('strikethrough', () => {
    expect(rt('text ~~gone~~ here\n')).toBe('text ~~gone~~ here\n')
  })

  it('image', () => {
    expect(rt('![alt](src.png)\n')).toBe('![alt](src.png)\n')
    expect(rt('![alt](src.png "Title")\n')).toBe('![alt](src.png "Title")\n')
  })

  it('image inside link', () => {
    const md = '[![alt](img.png)](https://example.com)\n'
    expect(rt(md)).toBe(md)
  })
})

describe('pm-schema escapeInline', () => {
  it.each([
    ['change_id stays unescaped', 'the change_id field\n', 'the change_id field\n'],
    ['intraword underscore safe', 'foo_bar_baz\n', 'foo_bar_baz\n'],
    ['__dunder__ escapes outer', '\\_\\_init\\_\\_\n', '\\_\\_init\\_\\_\n'],
    ['leading _word escaped', '\\_foo bar\n', '\\_foo bar\n'],
    ['a*b escapes star', 'a\\*b\n', 'a\\*b\n'],
    ['] not escaped, [ escaped', 'see \\[ref] here\n', 'see \\[ref] here\n'],
    ['~~ escaped, single ~ not', 'a~b \\~~c\n', 'a~b \\~~c\n'],
  ])('%s', (_label, md, expected) => {
    expect(rt(md)).toBe(expected)
  })

  it('the headline case: change_id round-trips clean', () => {
    const md = 'Use `change_id` not commit_id for identity.\n'
    expect(rt(md)).toBe(md)
  })

  it('literal ! before a link does not become an image', () => {
    // Input: escaped-! then link. Output must keep the escape so re-parse
    // doesn't see ![...](...).
    const md = '\\![text](url)\n'
    const out = rt(md)
    expect(out).toBe(md)
    expect(rt(out)).toBe(out)
  })
})

describe('pm-schema unit', () => {
  it('heading + paragraph + marks', () => {
    const md = '# Title\n\npara **bold** and *em* and `code` and [link](url)\n'
    expect(rt(md)).toBe(md)
  })

  it('nested bullet list', () => {
    const md = '- a\n  - nested\n- b\n'
    expect(rt(md)).toBe(md)
  })

  it('ordered list preserves start', () => {
    const md = '3. third\n4. fourth\n'
    expect(rt(md)).toBe(md)
  })

  it('fenced code block with lang', () => {
    const md = '```js\nconst s = `x`\n```\n'
    expect(rt(md)).toBe(md)
  })

  it('code span with internal backtick uses symmetric N+1 delimiters', () => {
    expect(rt('``foo`bar`` here\n')).toBe('``foo`bar`` here\n')
    expect(rt('``` `` ``` more\n')).toBe('``` `` ``` more\n')
  })

  it('code fence grows when content has line-start backticks', () => {
    const md = '````\n```\nnested fence\n```\n````\n'
    expect(rt(md)).toBe(md)
  })

  it('task list', () => {
    const md = '- [ ] todo\n- [x] done\n'
    expect(rt(md)).toBe(md)
  })

  it('blockquote', () => {
    const md = '> quoted **text**\n> \n> second para\n'
    expect(rt(md)).toBe(md)
  })

  it('link with title', () => {
    const md = '[text](http://x "Title")\n'
    expect(rt(md)).toBe(md)
  })

  it('link title with quote escapes', () => {
    const md = '[x](url "say \\"hi\\"")\n'
    expect(rt(md)).toBe(md)
  })

  it('href with space wraps in angle brackets', () => {
    const md = '[x](<http://a/b c>)\n'
    expect(rt(md)).toBe(md)
  })

  it('paragraph starting N) escapes so it does not become a list', () => {
    // Can't test via rt() — markdown source `1) foo` IS a list. Build the PM
    // doc directly to simulate the user typing `1) foo` in WYSIWYG.
    const p = docSchema.node('paragraph', null, [docSchema.text('1) not a list')])
    const out = serializeMarkdown(docSchema.node('doc', null, [p]))
    expect(out).toBe('\\1) not a list\n')
    expect(parseMarkdown(out).firstChild?.type.name).toBe('paragraph')
  })

  it('hr', () => {
    expect(rt('---\n')).toBe('---\n')
  })

  it('hard break', () => {
    expect(rt('a\\\nb\n')).toBe('a\\\nb\n')
  })

  it('idempotent: rt(rt(x)) === rt(x)', () => {
    const samples = [
      '# H\n\n- a\n- b\n',
      '```\ncode\n```\n',
      '> a\n',
      'plain *em* text\n',
    ]
    for (const s of samples) {
      const once = rt(s)
      expect(rt(once)).toBe(once)
    }
  })

  it('parses to a valid PM doc (schema check)', () => {
    const doc = parseMarkdown('# H\n\n- a\n  - b\n\n```js\nx\n```\n')
    expect(() => doc.check()).not.toThrow()
    expect(doc.type).toBe(docSchema.nodes.doc)
  })
})

describe('pm-schema round-trip on real docs', () => {
  const root = join(__dirname, '../../..')
  const docs = [
    'README.md',
    'BACKLOG.md',
    'docs/ARCHITECTURE.md',
    'docs/CONFIG.md',
    'docs/ANNOTATIONS.md',
  ].filter((p) => existsSync(join(root, p)))

  function lineDiff(a: string, b: string) {
    const al = a.split('\n')
    const bl = b.split('\n')
    let same = 0
    const max = Math.max(al.length, bl.length)
    for (let i = 0; i < max; i++) if (al[i] === bl[i]) same++
    // Content metric: source lines that don't appear ANYWHERE in output —
    // ignores position shifts from blank-line normalization, surfaces real
    // content changes (escaping, table reflow).
    const bset = new Set(bl)
    const lost = al.filter((ln) => !bset.has(ln)).length
    return { total: max, same, diff: max - same, lost }
  }

  for (const path of docs) {
    it(`${path}`, () => {
      const src = readFileSync(join(root, path), 'utf8')
      const once = rt(src)
      const twice = rt(once)
      const d = lineDiff(src, once)
      // Report-only: log stats; assert idempotence + non-empty
      // eslint-disable-next-line no-console
      console.log(
        `[round-trip] ${path}: ${d.total} lines, ${d.diff} pos-differ ` +
          `(${((100 * d.diff) / d.total).toFixed(1)}%), ${d.lost} content-changed ` +
          `(${((100 * d.lost) / d.total).toFixed(1)}%), idempotent=${once === twice}`
      )
      expect(once.length).toBeGreaterThan(0)
      expect(twice).toBe(once) // idempotence is the hard requirement
    })
  }
})
