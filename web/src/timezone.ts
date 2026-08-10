import { useSyncExternalStore } from 'react'

// Timezone display store, mirroring theme.ts. UTC is the default on purpose
// (ops-tool convention: agents span many timezones and incident times must
// read the same on every screen); a stored 'local' is the only opt-out, so
// an unreadable or garbage localStorage value always lands on UTC. Unlike
// theme there is no pre-paint script — nothing renders a time before React.
export type TZMode = 'utc' | 'local'

const STORAGE_KEY = 'polarbeam-timezone'
const listeners = new Set<() => void>()

function readMode(): TZMode {
  try {
    if (localStorage.getItem(STORAGE_KEY) === 'local') return 'local'
  } catch {
    /* storage unavailable; behave as UTC */
  }
  return 'utc'
}

let mode: TZMode = readMode()

export function getTZMode(): TZMode {
  return mode
}

export function setTZMode(next: TZMode) {
  mode = next
  try {
    localStorage.setItem(STORAGE_KEY, next)
  } catch {
    /* preference just won't survive reload */
  }
  for (const l of listeners) l()
}

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

// Components that render absolute times (directly or via fmtTime) subscribe
// through this hook so a toggle re-renders them — fmtTime reading module
// state does not, and relying on an App-root re-render would silently break
// under memoized subtrees.
export function useTimezone(): { mode: TZMode; setMode: (m: TZMode) => void } {
  const m = useSyncExternalStore(subscribe, getTZMode)
  return { mode: m, setMode: setTZMode }
}
