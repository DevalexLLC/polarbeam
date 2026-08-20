// The TypeScript half of the cross-language threshold-merge fence.
//
// It reads the SAME case table the Go test reads
// (internal/server/thresholds/testdata/threshold-merge.json) and asserts
// web/src/severity.ts resolves every case identically. Change one resolver
// without the other and exactly one of the two CI jobs fails: this one in
// web-lint, its counterpart in offline-build.
//
// Deliberately dependency-free. Node 24 (web/.nvmrc) strips TypeScript types
// natively, and severity.ts imports only types from ./types, so this runs
// with no test runner and no devDependency — which matters because a direct
// devDependency moves the lockfile and can widen the attribution walk in
// gen-spa-licenses.mjs. It also means the check works before pnpm install.
//
// Run: node web/tools/check-threshold-merge.ts   (from the repo root or web/)

import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { mergeLayers } from '../src/severity.ts'
import type { ThresholdOverrideFields, ThresholdSettings } from '../src/types.ts'

const here = dirname(fileURLToPath(import.meta.url))
const fixture = resolve(here, '../../internal/server/thresholds/testdata/threshold-merge.json')

interface Case {
  name: string
  pair_network: ThresholdOverrideFields | null
  pair_all: ThresholdOverrideFields | null
  network: ThresholdOverrideFields | null
  expect: ThresholdSettings
}

const data = JSON.parse(readFileSync(fixture, 'utf8')) as {
  global: ThresholdSettings
  cases: Case[]
}

if (!data.cases?.length) {
  console.error('check-threshold-merge: FATAL — fixture has no cases')
  process.exit(1)
}

const KEYS = [
  'latency_warn_us',
  'latency_crit_us',
  'loss_warn_pct',
  'loss_crit_pct',
] as const satisfies readonly (keyof ThresholdSettings)[]

let failed = 0
for (const c of data.cases) {
  // Specificity order, most specific first — the same order ingest and
  // httpapi pass, and the same order buildThresholdResolver folds.
  const got = mergeLayers(data.global, c.pair_network ?? undefined, c.pair_all ?? undefined, c.network ?? undefined)
  for (const k of KEYS) {
    if (got[k] !== c.expect[k]) {
      console.error(`check-threshold-merge: ${c.name}: ${k} = ${got[k]}, want ${c.expect[k]}`)
      failed++
    }
  }
}

if (failed > 0) {
  console.error(
    `check-threshold-merge: ${failed} mismatch(es) — the SPA resolver and ` +
      'internal/server/thresholds disagree; the dashboard and the outage ' +
      'detector would grade the same measurement differently.',
  )
  process.exit(1)
}
console.log(`check-threshold-merge: ${data.cases.length} cases OK`)
