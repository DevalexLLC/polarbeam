import { useEffect, useState } from 'react'
import { apiGet } from '../api'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
import type { LoginMonth, UsersResponse } from '../types'

const POLL_MS = 30_000

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

// 12 monthly sign-in totals as stacked bars, local under SSO (hand-rolled
// SVG — uPlot is for real charts). The hover card is fixed-position like
// HealthStrip's: the chart sits inside a card that would clip an absolute
// child near the top edge.
function LoginBars({ months }: { months: LoginMonth[] }) {
  const [hover, setHover] = useState<{ i: number; x: number; y: number; below: boolean } | null>(null)
  const max = Math.max(1, ...months.map((m) => m.total))
  const n = months.length

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect()
    const i = Math.min(n - 1, Math.max(0, Math.floor(((e.clientX - r.left) / r.width) * n)))
    const x = Math.min(Math.max(r.left + ((i + 0.5) / n) * r.width, 140), window.innerWidth - 140)
    const below = r.top < 150
    const y = below ? r.bottom : r.top
    setHover((prev) =>
      prev && prev.i === i && prev.x === x && prev.y === y && prev.below === below ? prev : { i, x, y, below },
    )
  }

  const m = hover ? months[hover.i] : null
  return (
    <>
      {/* Hover-only readout mirroring the aggregate aria-label, as on the
          fleet health strips. */}
      {/* oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions */}
      <svg
        className="login-bars"
        viewBox={`0 0 ${n * SLOT_W} ${CHART_H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={`Sign-ins per month: ${months.map((mo) => `${monthLabel(mo, true)} ${mo.total}`).join(', ')}`}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        {months.map((mo, i) => {
          const x = i * SLOT_W + (SLOT_W - BAR_W) / 2
          if (mo.total === 0) {
            // A zero month keeps a baseline tick so the axis stays readable.
            return <rect key={mo.month} className="login-bar-zero" x={x} y={BASELINE - 1} width={BAR_W} height={1} />
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
            {m.total}
            <small> {m.total === 1 ? 'sign-in' : 'sign-ins'}</small>
          </div>
          <div className="map-tip-caption">
            {m.total === 0
              ? 'no sign-ins this month'
              : `${m.local} local · ${m.oidc} SSO · ${m.unique_users} unique ${m.unique_users === 1 ? 'user' : 'users'}`}
          </div>
        </div>
      )}
    </>
  )
}

const PAGE_SIZE = 50

type RoleFilter = '' | 'admin' | 'viewer'
type StatusFilter = '' | 'active' | 'disabled' | 'deleted'
type SourceFilter = '' | 'local' | 'oidc'

// FilterGroup renders one segmented all-or-one control, the incident
// toolbar's control-group pattern.
function FilterGroup<T extends string>({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: T
  options: Array<{ value: T; label: string }>
  onChange: (v: T) => void
}) {
  return (
    <div className="control-group" role="group" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.value}
          className={value === o.value ? 'active' : ''}
          aria-pressed={value === o.value}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

export default function UsersPanel({
  isAdmin,
  onAuthError,
}: {
  isAdmin: boolean
  onAuthError: (err: unknown) => void
}) {
  useTimezone() // re-render fmtTime renders on UTC/local toggle
  const [data, setData] = useState<UsersResponse | null>(null)
  const [error, setError] = useState('')
  const [query, setQuery] = useState('') // raw input
  const [q, setQ] = useState('') // debounced, applied to the fetch
  const [role, setRole] = useState<RoleFilter>('')
  const [status, setStatus] = useState<StatusFilter>('')
  const [source, setSource] = useState<SourceFilter>('')
  const [offset, setOffset] = useState(0)

  // Debounce the search box so each keystroke doesn't hit the server; any
  // applied-filter change resets to the first page.
  useEffect(() => {
    const id = setTimeout(() => {
      setQ(query)
      setOffset(0)
    }, 300)
    return () => clearTimeout(id)
  }, [query])

  const applyFilter = <T,>(set: (v: T) => void) => {
    return (v: T) => {
      set(v)
      setOffset(0)
    }
  }

  // Self-heal an out-of-range page: if the matching set shrinks under the
  // current offset (deletion, or another admin's change landing via the
  // poll), the response is an empty window with a nonzero total — snap
  // back to the last valid page instead of showing a misleading empty
  // state with no pager.
  useEffect(() => {
    if (data && data.total > 0 && offset >= data.total) {
      setOffset(Math.floor((data.total - 1) / PAGE_SIZE) * PAGE_SIZE)
    }
  }, [data, offset])

  // Like the token list, this GET is admin-only (usernames, roles, and
  // sign-in history), so viewers get a static explanation instead of a
  // doomed fetch.
  useEffect(() => {
    if (!isAdmin) return
    let cancelled = false
    const params = new URLSearchParams()
    if (q) params.set('q', q)
    if (role) params.set('role', role)
    if (status) params.set('status', status)
    if (source) params.set('source', source)
    params.set('limit', String(PAGE_SIZE))
    if (offset > 0) params.set('offset', String(offset))
    const url = '/api/v1/users?' + params.toString()
    const load = () => {
      apiGet<UsersResponse>(url)
        .then((res) => {
          if (!cancelled) {
            setData(res)
            setError('')
          }
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          setError(err instanceof Error ? err.message : String(err))
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [isAdmin, onAuthError, q, role, status, source, offset])

  if (!isAdmin) {
    return (
      <div className="state-panel">
        <h2>Admin role required</h2>
        <p>User accounts and sign-in activity are visible to administrators only.</p>
      </div>
    )
  }
  if (error && !data) {
    return (
      <div className="state-panel state-error">
        <h2>User accounts unavailable</h2>
        <p>{error}</p>
      </div>
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading user accounts…
      </div>
    )
  }

  return (
    <>
      {error && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Sign-in activity</span>
            <h2>Last 12 months</h2>
          </div>
          <span className="hint">UTC calendar months · refreshes every 30s</span>
        </div>
        <LoginBars months={data.login_months} />
        <div className="login-bars-legend">
          <span>
            <span className="swatch chart-local" /> Local
          </span>
          <span>
            <span className="swatch chart-oidc" /> SSO
          </span>
        </div>
      </section>
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Accounts</span>
            <h2>Dashboard users</h2>
          </div>
        </div>
        <p className="section-intro">
          Local accounts are created with <code>polarbeam-server user add</code>; single sign-on accounts are
          provisioned automatically at first login. Deleted accounts stay listed with their last-known details as long
          as their sign-in history is retained. Sign-in counts start when this server first records logins.
        </p>
        <div className="users-toolbar">
          <label className="search-field">
            <span className="sr-only">Search usernames</span>
            <input
              type="search"
              placeholder="Search usernames"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
          </label>
          <FilterGroup<RoleFilter>
            label="Role"
            value={role}
            onChange={applyFilter(setRole)}
            options={[
              { value: '', label: 'All roles' },
              { value: 'admin', label: 'Admin' },
              { value: 'viewer', label: 'Viewer' },
            ]}
          />
          <FilterGroup<StatusFilter>
            label="Status"
            value={status}
            onChange={applyFilter(setStatus)}
            options={[
              { value: '', label: 'All statuses' },
              { value: 'active', label: 'Active' },
              { value: 'disabled', label: 'Disabled' },
              { value: 'deleted', label: 'Deleted' },
            ]}
          />
          <FilterGroup<SourceFilter>
            label="Source"
            value={source}
            onChange={applyFilter(setSource)}
            options={[
              { value: '', label: 'All sources' },
              { value: 'local', label: 'Local' },
              { value: 'oidc', label: 'SSO' },
            ]}
          />
        </div>
        {data.users.length === 0 ? (
          <div className="empty-state">
            {q || role || status || source ? (
              <>
                <strong>No matching accounts</strong>
                <span>Try a different username or widen the filters.</span>
              </>
            ) : (
              <>
                <strong>No user accounts</strong>
                <span>
                  Create one with <code>polarbeam-server user add</code>.
                </span>
              </>
            )}
          </div>
        ) : (
          <>
            <div className="scroll-x">
              <table className="events">
                <thead>
                  <tr>
                    <th>Username</th>
                    <th>Role</th>
                    <th>Source</th>
                    <th>Status</th>
                    <th>Sign-ins</th>
                    <th>Last sign-in</th>
                    <th>Created</th>
                  </tr>
                </thead>
                <tbody>
                  {data.users.map((u) => (
                    <tr key={u.id}>
                      <td data-label="Username" className="mono">
                        {u.username}
                      </td>
                      <td data-label="Role">{u.role === 'admin' ? 'Admin' : 'Viewer'}</td>
                      <td data-label="Source">{u.auth_source === 'oidc' ? 'SSO' : 'Local'}</td>
                      <td data-label="Status">
                        {u.status === 'disabled' ? (
                          <span className="status-text-down">Disabled</span>
                        ) : u.status === 'deleted' ? (
                          <span className="muted">Deleted</span>
                        ) : (
                          'Active'
                        )}
                      </td>
                      <td data-label="Sign-ins">{u.login_count}</td>
                      <td data-label="Last sign-in" title={u.last_login_at ? fmtTime(u.last_login_at) : undefined}>
                        {fmtAgo(u.last_login_at)}
                      </td>
                      <td data-label="Created" title={u.created_at ? fmtTime(u.created_at) : undefined}>
                        {u.created_at ? fmtAgo(u.created_at) : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {(data.total > PAGE_SIZE || offset > 0) && (
              <div className="users-pager">
                <span className="hint">
                  Showing {offset + 1}–{offset + data.users.length} of {data.total}
                </span>
                <span className="users-pager-buttons">
                  <button
                    type="button"
                    className="secondary-button"
                    disabled={offset === 0}
                    onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
                  >
                    Previous
                  </button>
                  <button
                    type="button"
                    className="secondary-button"
                    disabled={offset + data.users.length >= data.total}
                    onClick={() => setOffset(offset + PAGE_SIZE)}
                  >
                    Next
                  </button>
                </span>
              </div>
            )}
          </>
        )}
      </section>
    </>
  )
}
