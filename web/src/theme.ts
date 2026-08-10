import { useSyncExternalStore } from 'react'

// Theme store. public/theme-init.js sets data-theme before first paint; this
// module owns it from then on. The localStorage key stores only an explicit
// 'light'/'dark' preference — absence means "follow the OS scheme", so a
// system-mode user never has a stale value shadowing an OS change.
export type ThemePref = 'light' | 'dark' | 'system'
export type ResolvedTheme = 'light' | 'dark'

const STORAGE_KEY = 'polarbeam-theme'
const media = matchMedia('(prefers-color-scheme: dark)')
const listeners = new Set<() => void>()

function readPref(): ThemePref {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'light' || v === 'dark') return v
  } catch {
    /* storage unavailable; behave as system */
  }
  return 'system'
}

let pref: ThemePref = readPref()

export function resolvedTheme(): ResolvedTheme {
  if (pref !== 'system') return pref
  return media.matches ? 'dark' : 'light'
}

function apply() {
  document.documentElement.dataset.theme = resolvedTheme()
  for (const l of listeners) l()
}

media.addEventListener('change', () => {
  if (pref === 'system') apply()
})

export function getThemePref(): ThemePref {
  return pref
}

export function setThemePref(next: ThemePref) {
  pref = next
  try {
    if (next === 'system') localStorage.removeItem(STORAGE_KEY)
    else localStorage.setItem(STORAGE_KEY, next)
  } catch {
    /* preference just won't survive reload */
  }
  apply()
}

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

export function useTheme(): {
  pref: ThemePref
  resolved: ResolvedTheme
  setPref: (p: ThemePref) => void
} {
  const p = useSyncExternalStore(subscribe, getThemePref)
  const resolved = useSyncExternalStore(subscribe, resolvedTheme)
  return { pref: p, resolved, setPref: setThemePref }
}
