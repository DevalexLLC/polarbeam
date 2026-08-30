// The SPA's attentionReason (Overview.tsx, Agents.tsx) mirrors the server's
// needs_attention predicate (internal/server/store/inventory.go). The shared
// numbers live in one fixture the Go side also reads
// (internal/server/store/testdata/attention-parity.json, checked by
// store/attention_parity_test.go); this test pins the TS constants to it,
// source-text style like a11y-source.test.mjs, because the test env is
// DOM-free.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const fixture = JSON.parse(
  readFileSync(join(root, '..', 'internal', 'server', 'store', 'testdata', 'attention-parity.json'), 'utf8'),
)

function constant(source, name) {
  const m = source.match(new RegExp(`const ${name} = ([0-9_*\\s]+)`))
  assert.ok(m, `${name} constant not found`)
  // eslint-disable-next-line no-new-func -- literal arithmetic from our own source
  return Function(`return (${m[1]})`)()
}

test('attention thresholds match the shared server fixture', () => {
  for (const view of ['Overview.tsx', 'Agents.tsx']) {
    const source = readFileSync(join(root, 'src', 'views', view), 'utf8')
    assert.equal(
      constant(source, 'CERT_WARN_DAYS'),
      fixture.cert_warn_days,
      `${view}: CERT_WARN_DAYS diverges from attention-parity.json`,
    )
  }
  const overview = readFileSync(join(root, 'src', 'views', 'Overview.tsx'), 'utf8')
  assert.equal(
    constant(overview, 'DROP_ATTENTION_MS'),
    fixture.drop_attention_hours * 60 * 60 * 1000,
    'Overview.tsx: DROP_ATTENTION_MS diverges from attention-parity.json',
  )
})
