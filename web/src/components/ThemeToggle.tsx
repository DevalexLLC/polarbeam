import { useTheme } from '../theme'
import type { ThemePref } from '../theme'

const NEXT: Record<ThemePref, ThemePref> = { light: 'dark', dark: 'system', system: 'light' }
const LABEL: Record<ThemePref, string> = {
  light: 'Light theme',
  dark: 'Dark theme',
  system: 'System theme',
}

function Icon({ pref }: { pref: ThemePref }) {
  if (pref === 'light') {
    return (
      <svg className="theme-toggle-icon" viewBox="0 0 24 24" aria-hidden="true">
        <circle cx="12" cy="12" r="4" />
        <path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6l1.4 1.4M17 17l1.4 1.4M18.4 5.6L17 7M7 17l-1.4 1.4" />
      </svg>
    )
  }
  if (pref === 'dark') {
    return (
      <svg className="theme-toggle-icon" viewBox="0 0 24 24" aria-hidden="true">
        <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4 8.5 8.5 0 1 0 20 14.5Z" />
      </svg>
    )
  }
  return (
    <svg className="theme-toggle-icon" viewBox="0 0 24 24" aria-hidden="true">
      <rect x="3.5" y="5" width="17" height="12" rx="1.5" />
      <path d="M9.5 20h5M12 17v3" />
    </svg>
  )
}

export default function ThemeToggle() {
  const { pref, setPref } = useTheme()
  const label = `${LABEL[pref]} — switch to ${LABEL[NEXT[pref]].toLowerCase()}`
  return (
    <button type="button" className="theme-toggle" aria-label={label} title={label} onClick={() => setPref(NEXT[pref])}>
      <Icon pref={pref} />
    </button>
  )
}
