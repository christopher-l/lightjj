// Minimal ambient typings for the handful of Node APIs our tests touch
// (pm-schema.test.ts and themes.test.ts read fixture files off disk).
// Deliberately NOT @types/node: that package (plus its undici-types
// transitive) would type-check nothing else here — vitest runs the code,
// this only has to satisfy svelte-check. Extend when a test needs more.
declare module 'node:fs' {
  export function readFileSync(path: string, encoding: 'utf8' | 'utf-8'): string
  export function readFileSync(path: string): Uint8Array
  export function existsSync(path: string): boolean
}
declare module 'node:path' {
  export function join(...parts: string[]): string
}
declare const __dirname: string
