import assert from 'node:assert/strict'
import test from 'node:test'
import { documentTitle, isTargetID, pageFailure } from '../src/pageState.ts'

test('document titles consistently include the product name', () => {
  assert.equal(documentTitle(), 'PolarBEAM')
  assert.equal(documentTitle('Incidents'), 'Incidents · PolarBEAM')
  assert.equal(documentTitle('Sites · Settings'), 'Sites · Settings · PolarBEAM')
})

test('target detail accepts UUID identities only', () => {
  assert.equal(isTargetID('2f2a264e-0d9f-4fc7-8032-41d00448e278'), true)
  assert.equal(isTargetID('2F2A264E-0D9F-4FC7-8032-41D00448E278'), true)
  assert.equal(isTargetID('2f2a264e0d9f4fc7803241d00448e278'), true)
  assert.equal(isTargetID('{2f2a264e-0d9f-4fc7-8032-41d00448e278}'), true)
  assert.equal(isTargetID('urn:uuid:2f2a264e-0d9f-4fc7-8032-41d00448e278'), true)
  assert.equal(isTargetID('not-a-target'), false)
  assert.equal(isTargetID('{2f2a264e0d9f4fc7803241d00448e278}'), false)
  assert.equal(isTargetID(''), false)
})

test('page failures expose recovery policy without diagnostic causes', () => {
  assert.deepEqual(pageFailure({ status: 404, message: 'sql: no rows' }, 'target'), {
    message: 'The requested target could not be found.',
    retryable: false,
  })
  assert.deepEqual(pageFailure({ status: 503, message: 'dial tcp 10.0.0.2' }, 'target'), {
    message: 'PolarBEAM could not load the target. Try again.',
    retryable: true,
  })
  assert.equal(pageFailure({ status: 403 }, 'page').retryable, false)
  assert.equal(pageFailure(new TypeError('Failed to fetch'), 'page').retryable, true)
})
