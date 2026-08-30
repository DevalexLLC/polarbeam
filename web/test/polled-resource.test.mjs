// Pure unit tests for the fetch/poll controller behind usePolledResource.
// DOM-free: timers are injected as a recorder and fetches are manually
// resolved deferreds, so cancellation and interval logic run without React.
import assert from 'node:assert/strict'
import test from 'node:test'
import { POLL_MS, startPolledResource } from '../src/polledResource.ts'

const makeTimers = () => {
  const intervals = []
  return {
    intervals,
    timers: {
      setInterval(fn, ms) {
        const entry = { fn, ms, cleared: false }
        intervals.push(entry)
        return entry
      },
      clearInterval(id) {
        id.cleared = true
      },
    },
  }
}

const makeFetcher = () => {
  const calls = []
  const fetcher = () =>
    new Promise((resolve, reject) => {
      calls.push({ resolve, reject })
    })
  return { fetcher, calls }
}

const makeRecorder = () => {
  const events = []
  return {
    events,
    callbacks: {
      onData: (data) => events.push(['data', data]),
      onError: (err) => events.push(['error', err]),
      onAuthError: (err) => events.push(['auth', err]),
      logError: (err) => events.push(['log', err]),
      onRefreshing: (on) => events.push(['refreshing', on]),
    },
  }
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0))

test('the shared poll cadence is 30 seconds', () => {
  assert.equal(POLL_MS, 30_000)
})

test('starting fetches immediately and commits the response', async () => {
  const { fetcher, calls } = makeFetcher()
  const { events, callbacks } = makeRecorder()
  const { timers } = makeTimers()
  startPolledResource(fetcher, POLL_MS, callbacks, timers)
  assert.equal(calls.length, 1)
  calls[0].resolve({ ok: 1 })
  await settle()
  assert.deepEqual(events, [
    ['refreshing', true],
    ['data', { ok: 1 }],
    ['refreshing', false],
  ])
})

test('the interval registers at pollMs and each tick refetches', () => {
  const { fetcher, calls } = makeFetcher()
  const { callbacks } = makeRecorder()
  const { intervals, timers } = makeTimers()
  startPolledResource(fetcher, POLL_MS, callbacks, timers)
  assert.equal(intervals.length, 1)
  assert.equal(intervals[0].ms, POLL_MS)
  intervals[0].fn()
  assert.equal(calls.length, 2)
})

test('a null or zero pollMs fetches once and registers no interval', () => {
  const { fetcher, calls } = makeFetcher()
  const { callbacks } = makeRecorder()
  const { intervals, timers } = makeTimers()
  startPolledResource(fetcher, null, callbacks, timers)
  startPolledResource(fetcher, 0, callbacks, timers)
  assert.equal(calls.length, 2)
  assert.equal(intervals.length, 0)
})

test('a superseded response never commits, in either resolution order', async () => {
  for (const newerFirst of [true, false]) {
    const { fetcher, calls } = makeFetcher()
    const { events, callbacks } = makeRecorder()
    const { timers } = makeTimers()
    const controller = startPolledResource(fetcher, null, callbacks, timers)
    void controller.reload()
    assert.equal(calls.length, 2)
    if (newerFirst) {
      calls[1].resolve('fresh')
      await settle()
      calls[0].resolve('stale')
    } else {
      calls[0].resolve('stale')
      await settle()
      calls[1].resolve('fresh')
    }
    await settle()
    const commits = events.filter(([kind]) => kind === 'data')
    assert.deepEqual(commits, [['data', 'fresh']])
  }
})

test('stop clears the interval and drops the in-flight response', async () => {
  const { fetcher, calls } = makeFetcher()
  const { events, callbacks } = makeRecorder()
  const { intervals, timers } = makeTimers()
  const controller = startPolledResource(fetcher, POLL_MS, callbacks, timers)
  controller.stop()
  assert.equal(intervals[0].cleared, true)
  calls[0].resolve('late')
  await settle()
  assert.deepEqual(events, [['refreshing', true]])
})

test('reload after stop is inert', async () => {
  const { fetcher, calls } = makeFetcher()
  const { callbacks } = makeRecorder()
  const { intervals, timers } = makeTimers()
  const controller = startPolledResource(fetcher, POLL_MS, callbacks, timers)
  controller.stop()
  await controller.reload()
  assert.equal(calls.length, 1)
  assert.equal(intervals.length, 1)
})

test('onAuthError and logError fire even for superseded failures; onError only for fresh ones', async () => {
  const { fetcher, calls } = makeFetcher()
  const { events, callbacks } = makeRecorder()
  const { timers } = makeTimers()
  const controller = startPolledResource(fetcher, null, callbacks, timers)
  void controller.reload()
  const stale = new Error('stale failure')
  const fresh = new Error('fresh failure')
  calls[0].reject(stale)
  await settle()
  assert.deepEqual(
    events.filter(([kind]) => kind !== 'refreshing'),
    [
      ['auth', stale],
      ['log', stale],
    ],
  )
  calls[1].reject(fresh)
  await settle()
  assert.deepEqual(
    events.filter(([kind]) => kind !== 'refreshing'),
    [
      ['auth', stale],
      ['log', stale],
      ['auth', fresh],
      ['log', fresh],
      ['error', fresh],
    ],
  )
})

test('reload resets the interval phase and settles with the load', async () => {
  const { fetcher, calls } = makeFetcher()
  const { callbacks } = makeRecorder()
  const { intervals, timers } = makeTimers()
  const controller = startPolledResource(fetcher, POLL_MS, callbacks, timers)
  const loaded = controller.reload()
  assert.equal(intervals[0].cleared, true)
  assert.equal(intervals.length, 2)
  assert.equal(intervals[1].ms, POLL_MS)
  let settledFlag = false
  void loaded.then(() => {
    settledFlag = true
  })
  calls[1].resolve('fresh')
  await settle()
  assert.equal(settledFlag, true)
})

test('refreshing stays true until the latest generation settles', async () => {
  const { fetcher, calls } = makeFetcher()
  const { events, callbacks } = makeRecorder()
  const { timers } = makeTimers()
  const controller = startPolledResource(fetcher, null, callbacks, timers)
  void controller.reload()
  calls[0].resolve('stale')
  await settle()
  assert.equal(
    events.some(([kind, on]) => kind === 'refreshing' && on === false),
    false,
  )
  calls[1].resolve('fresh')
  await settle()
  assert.deepEqual(events.at(-1), ['refreshing', false])
})

test('a failed load does not stop subsequent ticks', async () => {
  const { fetcher, calls } = makeFetcher()
  const { events, callbacks } = makeRecorder()
  const { intervals, timers } = makeTimers()
  startPolledResource(fetcher, POLL_MS, callbacks, timers)
  calls[0].reject(new Error('transient'))
  await settle()
  intervals[0].fn()
  assert.equal(calls.length, 2)
  calls[1].resolve('recovered')
  await settle()
  assert.deepEqual(events.at(-2), ['data', 'recovered'])
})
