import { useEffect, useRef, useState } from 'react'
import type { LoginMonth } from '../types'

const MONTH_NAMES = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

// Months arrive as "YYYY-MM" strings naming UTC calendar months; deriving
// labels from the string (never via Date) keeps a browser timezone from
// shifting a bucket into the neighboring month.
function monthLabel(m: LoginMonth, long: boolean): string {
  const year = m.month.slice(0, 4)
  const name = MONTH_NAMES[Number(m.month.slice(5, 7)) - 1] ?? m.month
  return long ? `${name} ${year}` : name
}

// Geometry in viewBox units (the SVG stretches to the card width).
const SLOT_W = 20
const BAR_W = 14
const CHART_H = 80
const BASELINE = CHART_H - 2
const MAX_BAR = CHART_H - 8

// Which monthly series the chart shows: sign-ins (stacked local under SSO)
// or active users (people who made any request that month — a session
// outlives a month boundary, so sign-ins alone undercount usage).
export type LoginBarsMode = 'active' | 'signins'

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`
}

function monthTotal(m: LoginMonth, mode: LoginBarsMode): number {
  return mode === 'active' ? m.active_users : m.total
}

function monthCaption(m: LoginMonth, mode: LoginBarsMode): string {
  if (mode === 'active') {
    return m.active_users === 0
      ? 'no activity this month'
      : `${m.unique_users} signed in · ${plural(m.total, 'sign-in', 'sign-ins')}`
  }
  return m.total === 0
    ? 'no sign-ins this month'
    : `${m.local} local · ${m.oidc} SSO · ${plural(m.unique_users, 'unique user', 'unique users')}`
}

// 12 monthly totals as bars (hand-rolled SVG — uPlot is for real charts).
// The hover card is fixed-position like HealthStrip's: the chart sits
// inside a card that would clip an absolute child near the top edge.
export default function LoginBars({ months, mode }: { months: LoginMonth[]; mode: LoginBarsMode }) {
  const [hover, setHover] = useState<{ i: number; x: number; y: number; below: boolean } | null>(null)
  const svgRef = useRef<SVGSVGElement>(null)
  const max = Math.max(1, ...months.map((m) => monthTotal(m, mode)))
  const n = months.length
  const noun = mode === 'active' ? 'Active users' : 'Sign-ins'

  // Hover-only readout mirroring the aggregate aria-label, as on the fleet
  // health strips. The listeners attach natively so the labeled svg stays a
  // plain image to assistive technology; the sr-only breakdown below is the
  // non-visual equivalent of what hovering reveals.
  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    const onMove = (e: MouseEvent) => {
      const r = svg.getBoundingClientRect()
      const i = Math.min(n - 1, Math.max(0, Math.floor(((e.clientX - r.left) / r.width) * n)))
      const x = Math.min(Math.max(r.left + ((i + 0.5) / n) * r.width, 140), window.innerWidth - 140)
      const below = r.top < 150
      const y = below ? r.bottom : r.top
      setHover((prev) =>
        prev && prev.i === i && prev.x === x && prev.y === y && prev.below === below ? prev : { i, x, y, below },
      )
    }
    const onLeave = () => setHover(null)
    svg.addEventListener('mousemove', onMove)
    svg.addEventListener('mouseleave', onLeave)
    return () => {
      svg.removeEventListener('mousemove', onMove)
      svg.removeEventListener('mouseleave', onLeave)
    }
  }, [n])

  const m = hover ? months[hover.i] : null
  return (
    <>
      <svg
        ref={svgRef}
        className="login-bars"
        viewBox={`0 0 ${n * SLOT_W} ${CHART_H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={`${noun} per month: ${months.map((mo) => `${monthLabel(mo, true)} ${monthTotal(mo, mode)}`).join(', ')}`}
      >
        {months.map((mo, i) => {
          const x = i * SLOT_W + (SLOT_W - BAR_W) / 2
          const total = monthTotal(mo, mode)
          if (total === 0) {
            // A zero month keeps a baseline tick so the axis stays readable.
            return <rect key={mo.month} className="login-bar-zero" x={x} y={BASELINE - 1} width={BAR_W} height={1} />
          }
          if (mode === 'active') {
            const h = (total / max) * MAX_BAR
            return (
              <rect key={mo.month} className="login-bar-local" x={x} y={BASELINE - h} width={BAR_W} height={h} rx={1} />
            )
          }
          const localH = (mo.local / max) * MAX_BAR
          const oidcH = (mo.oidc / max) * MAX_BAR
          // A 1-unit surface gap separates the stacked segments when both
          // are present.
          const gap = mo.local > 0 && mo.oidc > 0 ? 1 : 0
          return (
            <g key={mo.month}>
              {mo.local > 0 && (
                <rect className="login-bar-local" x={x} y={BASELINE - localH} width={BAR_W} height={localH} rx={1} />
              )}
              {mo.oidc > 0 && (
                <rect
                  className="login-bar-oidc"
                  x={x}
                  y={BASELINE - localH - gap - oidcH}
                  width={BAR_W}
                  height={oidcH}
                  rx={1}
                />
              )}
            </g>
          )
        })}
      </svg>
      <div className="login-bars-labels" aria-hidden="true">
        {months.map((mo, i) => (
          <span key={mo.month}>{monthLabel(mo, i === 0 || mo.month.endsWith('-01'))}</span>
        ))}
      </div>
      <p className="sr-only">
        {mode === 'active' ? 'Active users by month: ' : 'Sign-ins by month: '}
        {months
          .map((mo) => {
            const total = monthTotal(mo, mode)
            if (total === 0) return `${monthLabel(mo, true)}: ${mode === 'active' ? 'no activity' : 'no sign-ins'}`
            const head = mode === 'active' ? plural(total, 'active user', 'active users') : `${total} total`
            return `${monthLabel(mo, true)}: ${head}, ${monthCaption(mo, mode).replaceAll(' · ', ', ')}`
          })
          .join('; ')}
        .
      </p>
      {hover && m && (
        <div
          className={'map-tip strip-tip' + (hover.below ? ' strip-tip-below' : '')}
          role="status"
          style={{ left: hover.x, top: hover.y }}
        >
          <div className="map-tip-head">
            <b>{monthLabel(m, true)}</b>
          </div>
          <div className="map-tip-value">
            {monthTotal(m, mode)}
            <small>
              {' '}
              {mode === 'active'
                ? m.active_users === 1
                  ? 'active user'
                  : 'active users'
                : m.total === 1
                  ? 'sign-in'
                  : 'sign-ins'}
            </small>
          </div>
          <div className="map-tip-caption">{monthCaption(m, mode)}</div>
        </div>
      )}
    </>
  )
}
