import assert from 'node:assert/strict'
import test from 'node:test'
import { latestValueIndex } from '../src/chartkit.ts'

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
