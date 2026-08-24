import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('../src/views/Targets.tsx', import.meta.url), 'utf8')

test('operational targets use one stable-ID server inventory', () => {
  assert.match(source, /apiGet<OperationalTargetsResponse>\('\/api\/v1\/targets\?' \+ params\.toString\(\)\)/)
  assert.doesNotMatch(source, /\/api\/v1\/config\/(?:targets|probes)/)
  assert.doesNotMatch(source, /\/api\/v1\/agents/)
  assert.doesNotMatch(source, /\/api\/v1\/outages/)
  assert.match(source, /targetDetailHref\(t\.id\)/)
  assert.match(source, /t\.agent_site/)
  assert.match(source, /t\.probing_sites/)
})
