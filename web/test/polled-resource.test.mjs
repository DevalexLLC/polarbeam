// Pure unit tests for the fetch/poll controller behind usePolledResource.
// DOM-free: timers are injected as a recorder and fetches are manually
// resolved deferreds, so cancellation and interval logic run without React.
import assert from 'node:assert/strict'
import test from 'node:test'
import { keyedSnapshot, POLL_MS, startPolledResource } from '../src/polledResource.ts'

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

test('a failure after stop logs but never fires onAuthError', async () => {
  const { fetcher, calls } = makeFetcher()
  const { events, callbacks } = makeRecorder()
  const { timers } = makeTimers()
  const controller = startPolledResource(fetcher, POLL_MS, callbacks, timers)
  controller.stop()
  calls[0].reject(new Error('dead session'))
  await settle()
  assert.deepEqual(
    events.filter(([kind]) => kind !== 'refreshing').map(([kind]) => kind),
    ['log'],
  )
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

// keyedSnapshot: what a keyed view (pair/target detail) may render. The hook
// keeps data and error across key changes; each is trusted only under the
// key that produced it (#229).
const LAN = 'Alpha\u0000Beta\u000024h\u0000latency\u0000lan'
const WAN = 'Alpha\u0000Beta\u000024h\u0000latency\u0000wan'
const lanData = { samples: 123 }
const wanData = { samples: 456 }
const boom = new Error('boom')
const state = (over) => ({ data: null, error: null, loadedKey: undefined, errorKey: undefined, ...over })

test('keyedSnapshot: a slow switch shows loading, never the previous context', () => {
  const s = state({ data: lanData, loadedKey: LAN })
  assert.deepEqual(keyedSnapshot(s, WAN), { status: 'loading' })
})

test('keyedSnapshot: a failed switch reports the failure, never the previous context', () => {
  const s = state({ data: lanData, loadedKey: LAN, error: boom, errorKey: WAN })
  assert.deepEqual(keyedSnapshot(s, WAN), { status: 'failed', error: boom })
})

test('keyedSnapshot: a successful switch renders the new context', () => {
  const s = state({ data: wanData, loadedKey: WAN })
  assert.deepEqual(keyedSnapshot(s, WAN), { status: 'ready', data: wanData, stale: false })
})

test('keyedSnapshot: a same-context refresh failure keeps the snapshot, marked stale', () => {
  const s = state({ data: lanData, loadedKey: LAN, error: boom, errorKey: LAN })
  assert.deepEqual(keyedSnapshot(s, LAN), { status: 'ready', data: lanData, stale: true })
})

test("keyedSnapshot: the previous context's failure never marks the new one failed", () => {
  // The render right after a switch still holds the old context's error;
  // reporting it would flash (and focus) an error panel for a load that is
  // only starting.
  const s = state({ data: lanData, loadedKey: LAN, error: boom, errorKey: LAN })
  assert.deepEqual(keyedSnapshot(s, WAN), { status: 'loading' })
})

test('keyedSnapshot: switching back to the context that last loaded renders it at once', () => {
  // LAN -> WAN (never loaded) -> LAN: the retained snapshot is LAN's own.
  const s = state({ data: lanData, loadedKey: LAN })
  assert.deepEqual(keyedSnapshot(s, LAN), { status: 'ready', data: lanData, stale: false })
})

test('keyedSnapshot: nothing loaded yet is loading, even under the matching key', () => {
  assert.deepEqual(keyedSnapshot(state({ loadedKey: LAN }), LAN), { status: 'loading' })
  assert.deepEqual(keyedSnapshot(state({}), undefined), { status: 'loading' })
})

test('a failed plane switch never renders the previous plane through the controller', async () => {
  // Mirrors usePolledResource: one controller per key, callbacks stamp the
  // key that was current at start, stop() on every key change.
  const hook = state({})
  const mount = (key) => {
    const f = makeFetcher()
    const controller = startPolledResource(
      f.fetcher,
      POLL_MS,
      {
        onData: (data) => Object.assign(hook, { data, error: null, loadedKey: key }),
        onError: (error) => Object.assign(hook, { error, errorKey: key }),
      },
      makeTimers().timers,
    )
    return { calls: f.calls, controller }
  }

  const lan = mount(LAN)
  lan.calls[0].resolve(lanData)
  await settle()
  assert.deepEqual(keyedSnapshot(hook, LAN), { status: 'ready', data: lanData, stale: false })

  // A LAN poll is in flight when the filter moves to WAN.
  lan.controller.reload()
  lan.controller.stop()
  const wan = mount(WAN)
  assert.deepEqual(keyedSnapshot(hook, WAN), { status: 'loading' }, 'slow switch')

  wan.calls[0].reject(boom)
  lan.calls[1].resolve({ samples: 999 })
  await settle()
  assert.deepEqual(keyedSnapshot(hook, WAN), { status: 'failed', error: boom }, 'failed switch')
  assert.equal(hook.data, lanData, 'the late LAN response stays suppressed')

  wan.controller.reload()
  wan.calls[1].resolve(wanData)
  await settle()
  assert.deepEqual(keyedSnapshot(hook, WAN), { status: 'ready', data: wanData, stale: false }, 'retry')
})
