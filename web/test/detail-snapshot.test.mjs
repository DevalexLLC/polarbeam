// Source pins for #229: pair and target detail render only the hook's keyed
// snapshot, so a slow or failed context switch never shows the previous
// plane/window/metric under the new labels. keyedSnapshot's behavior is
// covered in polled-resource.test.mjs; these pin the wiring.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')
const hook = read('../src/usePolledResource.ts')
const pair = read('../src/views/PairDetail.tsx')
const target = read('../src/views/TargetDetail.tsx')
const app = read('../src/App.tsx')

test('the hook scopes errors to their key and returns the keyed snapshot', () => {
  assert.match(hook, /onError: \(err\) => \{\s*setError\(err\)\s*setErrorKey\(keyAtStart\)\s*\}/)
  assert.match(hook, /setLoadedKey\(undefined\)\s*setErrorKey\(undefined\)/)
  assert.match(hook, /const snapshot = keyedSnapshot\(\{ data, error, loadedKey, errorKey \}, key\)/)
})

for (const [name, src, key] of [
  ['pair detail', pair, /const requestKey = \[a, b, win, metric, net\]\.join\('\\u0000'\)/],
  ['target detail', target, /const requestKey = \[id, win, metric\]\.join\('\\u0000'\)/],
]) {
  test(`${name} renders only the snapshot for the selected context`, () => {
    assert.match(src, key)
    assert.match(src, /key: requestKey,/)
    assert.match(src, /snapshot: view,/)
    assert.match(src, /const current = view\.status === 'ready' \? view\.data : null/)
    // Every body field derives from the gated snapshot, never raw data.
    for (const field of ['series', 'settings', 'paths']) {
      assert.match(src, new RegExp(`const ${field} = current\\?\\.${field} \\?\\? null`), field)
      assert.doesNotMatch(src, new RegExp(`data\\?\\.${field}\\b`), field)
    }
    // The stale note belongs to a same-context refresh failure only; a
    // switch that failed shows the inline error, never "loading…" beside it.
    assert.match(src, /view\.status === 'ready' && view\.stale \? 'refresh failed, showing last data' : ''/)
    assert.match(src, /view\.status === 'loading' \? 'loading…' : ''/)
    assert.doesNotMatch(src, /\{error \?/)
    // After the first load, a switch keeps the head and controls mounted.
    assert.match(src, /\{head\}\s*<PageError\s*headingLevel=\{2\}/)
    assert.match(src, /\{head\}\s*<div className="state-panel" role="status">/)
  })
}

test('pair detail derives its summary from the gated snapshot', () => {
  assert.match(pair, /const pair = current\?\.pair \?\? null/)
  assert.doesNotMatch(pair, /data\?\.pair\b/)
})

test('target detail takes only its identity from the retained snapshot', () => {
  assert.match(target, /const summary = current\?\.summary \?\? null/)
  // The single ungated read: identity is per-id, and App remounts the view
  // per id, so a retained snapshot is always this target's.
  assert.equal(target.match(/data\?\.summary/g)?.length, 1)
  assert.match(target, /const identity = data\?\.summary\.target \?\? null/)
  assert.match(app, /<TargetDetail key=\{route\.id\}/)
  // The network filter stays client-side, out of the request key.
  assert.match(target, /matchesNetworkFilter\(network, s\.network\)/)
})
