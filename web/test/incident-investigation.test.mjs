import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const incidents = readFileSync(new URL('../src/views/Outages.tsx', import.meta.url), 'utf8')
const overview = readFileSync(new URL('../src/views/Overview.tsx', import.meta.url), 'utf8')

test('incident evidence is opt-in and uses the rendered snapshot window', () => {
  assert.match(incidents, /\/api\/v1\/outages\?window=\$\{win\}&include_routes=true/)
  assert.match(incidents, /win=\{snapshotWin\}/)
  assert.doesNotMatch(overview, /include_routes=true/)
})
