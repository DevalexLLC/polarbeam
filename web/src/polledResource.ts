// Pure fetch/poll controller behind usePolledResource. React-free so the
// cancellation and interval logic is unit-testable in the DOM-free suite:
// a single generation counter subsumes the per-effect `let cancelled` flags
// and the detail views' loadGen refs (superseded responses never commit).

export const POLL_MS = 30_000

export interface Timers {
  setInterval(fn: () => void, ms: number): unknown
  clearInterval(id: unknown): void
}

export interface PolledResourceCallbacks<T> {
  // Fresh (non-superseded) results only.
  onData(data: T): void
  onError(err: unknown): void
  // Every failure, even superseded ones: a 401 on a stale response must still
  // log the session out, and diagnostics should never be dropped silently.
  onAuthError?(err: unknown): void
  logError?(err: unknown): void
  // true when a load starts; false once the LATEST generation settles.
  onRefreshing?(refreshing: boolean): void
}

export interface PolledResourceController {
  // Immediate fresh load; supersedes in-flight loads and resets the interval
  // phase. The promise settles when this load's callbacks have run.
  reload(): Promise<void>
  // Clears the interval and drops in-flight responses.
  stop(): void
}

const globalTimers: Timers = {
  setInterval: (fn, ms) => setInterval(fn, ms),
  clearInterval: (id) => clearInterval(id as Parameters<typeof clearInterval>[0]),
}

export function startPolledResource<T>(
  fetcher: () => Promise<T>,
  pollMs: number | null,
  callbacks: PolledResourceCallbacks<T>,
  timers: Timers = globalTimers,
): PolledResourceController {
  let generation = 0
  let stopped = false
  let intervalID: unknown = null

  const load = (): Promise<void> => {
    const gen = ++generation
    callbacks.onRefreshing?.(true)
    return fetcher()
      .then((data) => {
        if (gen !== generation) return
        callbacks.onData(data)
      })
      .catch((err) => {
        callbacks.onAuthError?.(err)
        callbacks.logError?.(err)
        if (gen !== generation) return
        callbacks.onError(err)
      })
      .finally(() => {
        if (gen === generation) callbacks.onRefreshing?.(false)
      })
  }

  const startInterval = () => {
    if (pollMs) intervalID = timers.setInterval(() => void load(), pollMs)
  }
  const clearScheduled = () => {
    if (intervalID !== null) {
      timers.clearInterval(intervalID)
      intervalID = null
    }
  }

  void load()
  startInterval()

  return {
    reload: () => {
      if (stopped) return Promise.resolve()
      clearScheduled()
      startInterval()
      return load()
    },
    stop: () => {
      stopped = true
      generation++
      clearScheduled()
    },
  }
}
