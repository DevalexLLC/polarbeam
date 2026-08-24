import assert from 'node:assert/strict'
import test from 'node:test'
import { reconcileChartRange, xExtent } from '../src/chartRange.ts'

const selected = { min: 100, max: 200 }

test('appended samples preserve the absolute investigation range', () => {
  assert.deepEqual(reconcileChartRange(selected, { min: 0, max: 300 }), {
    mode: 'zoomed',
    range: selected,
    reason: 'preserved',
  })
})

test('partial overlap preserves the selected endpoints', () => {
  assert.deepEqual(reconcileChartRange(selected, { min: 150, max: 300 }), {
    mode: 'zoomed',
    range: selected,
    reason: 'partial-overlap',
  })
})

test('total expiry returns to live mode', () => {
  assert.deepEqual(reconcileChartRange(selected, { min: 201, max: 300 }), {
    mode: 'live',
    range: null,
    reason: 'expired',
  })
})

test('empty responses do not discard an investigation', () => {
  assert.deepEqual(reconcileChartRange(selected, null), {
    mode: 'zoomed',
    range: selected,
    reason: 'empty',
  })
  assert.equal(xExtent([]), null)
  assert.deepEqual(xExtent([null, Number.NaN, 7, 3, 11]), { min: 3, max: 11 })
})

test('context changes deterministically start a fresh live range', () => {
  assert.deepEqual(reconcileChartRange(selected, { min: 0, max: 300 }, true), {
    mode: 'live',
    range: null,
    reason: 'context-change',
  })
})
