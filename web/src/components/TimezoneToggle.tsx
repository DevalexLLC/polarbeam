import { useTimezone } from '../timezone'
import type { TZMode } from '../timezone'

const NEXT: Record<TZMode, TZMode> = { utc: 'local', local: 'utc' }
const LABEL: Record<TZMode, string> = { utc: 'Times in UTC', local: 'Times in local time' }
const TEXT: Record<TZMode, string> = { utc: 'UTC', local: 'Local' }

// Text label instead of an icon so the active frame of reference is always
// readable at a glance — the label IS the state.
export default function TimezoneToggle() {
  const { mode, setMode } = useTimezone()
  const label = `${LABEL[mode]} — switch to ${mode === 'utc' ? 'local time' : 'UTC'}`
  return (
    <button
      type="button"
      className="theme-toggle tz-toggle"
      aria-label={label}
      title={label}
      onClick={() => setMode(NEXT[mode])}
    >
      {TEXT[mode]}
    </button>
  )
}
