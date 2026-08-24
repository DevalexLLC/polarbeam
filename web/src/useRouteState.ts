import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import {
  canonicalizeRouteHash,
  routeHashSnapshot,
  routeNumberParam,
  routeParam,
  subscribeRouteState,
  updateRouteParams,
  type HistoryMode,
} from './routeState'

export function useRouteHash(): string {
  return useSyncExternalStore(subscribeRouteState, routeHashSnapshot)
}

export function useRouteParam(name: string, fallback = ''): [string, (value: string, mode?: HistoryMode) => void] {
  const hash = useRouteHash()
  const value = routeParam(hash, name) || fallback
  const setValue = useCallback(
    (next: string, mode: HistoryMode = 'push') => updateRouteParams({ [name]: next === fallback ? null : next }, mode),
    [fallback, name],
  )
  return [value, setValue]
}

export function useRouteNumber(name: string, fallback = 1): [number, (value: number, mode?: HistoryMode) => void] {
  const hash = useRouteHash()
  const value = routeNumberParam(hash, name, fallback)
  const setValue = useCallback(
    (next: number, mode: HistoryMode = 'push') => updateRouteParams({ [name]: next === fallback ? null : next }, mode),
    [fallback, name],
  )
  return [value, setValue]
}

// Search text updates the current history entry after the operator pauses,
// while Back/Forward immediately replaces the draft with the URL value.
export function useRouteSearch(name = 'q'): [string, (value: string) => void] {
  const hash = useRouteHash()
  const urlValue = canonicalizeRouteHash(hash).params.get(name) ?? ''
  const [draft, setDraft] = useState(urlValue)
  const lastWritten = useRef<string | null>(null)

  useEffect(() => {
    if (lastWritten.current === urlValue) {
      lastWritten.current = null
      return
    }
    setDraft(urlValue)
  }, [urlValue])
  useEffect(() => {
    if (draft === urlValue) return
    const timer = window.setTimeout(() => {
      lastWritten.current = draft
      updateRouteParams({ [name]: draft || null, page: null }, 'replace')
    }, 250)
    return () => window.clearTimeout(timer)
  }, [draft, name, urlValue])

  return [draft, setDraft]
}
