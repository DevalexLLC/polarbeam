import assert from 'node:assert/strict'
import test from 'node:test'
import { panChartRange, reconcileChartRange, xExtent, zoomChartRange } from '../src/chartRange.ts'

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

test('keyboard zoom in halves the span around the center', () => {
  assert.deepEqual(zoomChartRange({ min: 100, max: 200 }, 0.5, { min: 0, max: 1000 }), { min: 125, max: 175 })
})

test('keyboard zoom out past the extent returns null for a live reset', () => {
  assert.equal(zoomChartRange({ min: 100, max: 900 }, 2, { min: 0, max: 1000 }), null)
})

test('keyboard zoom clamps to the extent edges instead of overflowing', () => {
  assert.deepEqual(zoomChartRange({ min: 0, max: 100 }, 2, { min: 0, max: 1000 }), { min: 0, max: 200 })
  assert.deepEqual(zoomChartRange({ min: 900, max: 1000 }, 2, { min: 0, max: 1000 }), { min: 800, max: 1000 })
})

test('keyboard zoom never collapses below a thousandth of the extent', () => {
  assert.deepEqual(zoomChartRange({ min: 499, max: 501 }, 0.1, { min: 0, max: 1000 }), { min: 499.5, max: 500.5 })
})

test('keyboard pan shifts by a span fraction and clamps at the edges', () => {
  assert.deepEqual(panChartRange({ min: 100, max: 200 }, 0.2, { min: 0, max: 1000 }), { min: 120, max: 220 })
  assert.deepEqual(panChartRange({ min: 100, max: 200 }, -0.2, { min: 0, max: 1000 }), { min: 80, max: 180 })
  assert.deepEqual(panChartRange({ min: 950, max: 1050 }, 0.5, { min: 0, max: 1000 }), { min: 900, max: 1000 })
  assert.deepEqual(panChartRange({ min: 0, max: 100 }, -0.5, { min: 0, max: 1000 }), { min: 0, max: 100 })
})

test('keyboard pan with no data extent is a no-op', () => {
  assert.deepEqual(panChartRange({ min: 100, max: 200 }, 0.2, null), { min: 100, max: 200 })
})

test('keyboard pan of a range wider than the extent is a no-op', () => {
  // A preserved partial-overlap range can outlive the retained data;
  // clamping it would jump the same direction for both arrows.
  assert.deepEqual(panChartRange({ min: 100, max: 200 }, 0.2, { min: 150, max: 160 }), { min: 100, max: 200 })
  assert.deepEqual(panChartRange({ min: 100, max: 200 }, -0.2, { min: 150, max: 160 }), { min: 100, max: 200 })
})
