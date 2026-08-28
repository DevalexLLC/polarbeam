import assert from 'node:assert/strict'
import test from 'node:test'
import { latestValueIndex, summarizeSeries } from '../src/chartkit.ts'

test('latest legend value stays inside the visible investigation range', () => {
  const data = [
    [100, 150, 250],
    [1, 2, 3],
  ]
  assert.equal(latestValueIndex(data, { min: 100, max: 200 }), 1)
  assert.equal(latestValueIndex(data), 2)
})

test('latest legend value is empty when the visible range has no measurement', () => {
  const data = [
    [100, 150, 250],
    [null, null, 3],
  ]
  assert.equal(latestValueIndex(data, { min: 100, max: 200 }), null)
})

test('series summaries digest only the visible range and skip nulls', () => {
  const data = [
    [100, 150, 200, 250],
    [10, null, 30, 40],
    [null, null, null, null],
  ]
  assert.deepEqual(summarizeSeries(data, { min: 100, max: 200 }), [
    { count: 2, latest: 30, min: 10, max: 30, avg: 20 },
    { count: 0, latest: null, min: null, max: null, avg: null },
  ])
  assert.deepEqual(summarizeSeries(data, null)[0], { count: 3, latest: 40, min: 10, max: 40, avg: 80 / 3 })
})
