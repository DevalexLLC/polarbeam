// React wrapper over polledResource.ts: the SPA's one fetch/poll pattern.
// Views and panels pass a URL (or a fetcher for multi-request loads) and get
// the shared 30-second cadence, 401 -> onAuthError, per-view failure logging,
// and stale-response suppression without hand-rolling the effect.
import { useCallback, useEffect, useRef, useState } from 'react'
import { apiGet } from './api'
import { POLL_MS, startPolledResource, type PolledResourceController } from './polledResource'

export { POLL_MS }

export interface UsePolledResourceOptions {
  // Default POLL_MS; null (or 0) fetches once per key change with no interval.
  pollMs?: number | null
  // false skips fetching entirely (capability / tab / route-validity gates).
  enabled?: boolean
  // Refetch identity, compared with Object.is. Defaults to the URL when
  // source is a string, else never changes.
  key?: unknown
  // Clear data/error when the key changes or the gate closes (default: keep
  // the last snapshot while the next load is in flight).
  resetOnChange?: boolean
  onAuthError?: (err: unknown) => void
  // console.error(`${logLabel} request failed`, err) on every failure.
  logLabel?: string
  // Custom logger; overrides logLabel. Both omitted = silent.
  logError?: (err: unknown) => void
}

export interface PolledResource<T> {
  data: T | null
  error: unknown
  refreshing: boolean
  lastLoadedAt: Date | null
  // The key that was current when `data` was fetched.
  loadedKey: unknown
  reload: () => Promise<void>
}

export function usePolledResource<T>(
  source: string | (() => Promise<T>),
  options: UsePolledResourceOptions = {},
): PolledResource<T> {
  const { pollMs = POLL_MS, enabled = true, resetOnChange = false } = options
  const key = 'key' in options ? options.key : typeof source === 'string' ? source : undefined

  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [refreshing, setRefreshing] = useState(false)
  const [lastLoadedAt, setLastLoadedAt] = useState<Date | null>(null)
  const [loadedKey, setLoadedKey] = useState<unknown>(undefined)

  // Latest-value refs: the controller reads these so the effect only restarts
  // when the fetch identity (key/enabled/pollMs) changes, never because a
  // caller passed an inline fetcher or callback.
  const sourceRef = useRef(source)
  sourceRef.current = source
  const optionsRef = useRef(options)
  optionsRef.current = options
  const controllerRef = useRef<PolledResourceController | null>(null)

  useEffect(() => {
    if (!enabled) return
    const keyAtStart = key
    const controller = startPolledResource<T>(
      () => {
        const src = sourceRef.current
        return typeof src === 'string' ? apiGet<T>(src) : src()
      },
      pollMs,
      {
        onData: (res) => {
          setData(res)
          setError(null)
          setLastLoadedAt(new Date())
          setLoadedKey(keyAtStart)
        },
        onError: setError,
        onAuthError: (err) => optionsRef.current.onAuthError?.(err),
        logError: (err) => {
          const { logError, logLabel } = optionsRef.current
          if (logError) logError(err)
          else if (logLabel) console.error(`${logLabel} request failed`, err)
        },
        onRefreshing: setRefreshing,
      },
    )
    controllerRef.current = controller
    return () => {
      controllerRef.current = null
      controller.stop()
      if (resetOnChange) {
        setData(null)
        setError(null)
        setRefreshing(false)
        setLastLoadedAt(null)
        setLoadedKey(undefined)
      }
    }
  }, [key, enabled, pollMs, resetOnChange])

  const reload = useCallback(() => controllerRef.current?.reload() ?? Promise.resolve(), [])

  return { data, error, refreshing, lastLoadedAt, loadedKey, reload }
}
