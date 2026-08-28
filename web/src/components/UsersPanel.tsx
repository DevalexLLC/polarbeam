import { Fragment, useEffect, useId, useRef, useState } from 'react'
import { apiDelete, apiGet, apiPost, apiPut } from '../api'
import type { Caps } from '../caps'
import { roleLabel } from '../caps'
import { fmtAgo, fmtTime } from '../format'
import { useErrorSummary } from '../formErrors'
import { useConcurrentSettingsDraft, useSettingsDraft, useSettingsMutation } from '../settingsMutation'
import { useTimezone } from '../timezone'
import type { LoginMonth, Role, UserAccount, UserCreateResponse, UsersResponse } from '../types'
import ConfirmButton from './ConfirmButton'
import RoleWall from './RoleWall'
import SettingsPageError from './SettingsPageError'

const POLL_MS = 30_000

function orderedNetworks(values: string[]): string[] {
  // ES2023 toSorted is outside this project's browser target. The spread
  // is a fresh array, so this does not mutate React or API state.
  // oxlint-disable-next-line unicorn/no-array-sort
  return [...values].sort()
}

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
  const svgRef = useRef<SVGSVGElement>(null)
  const max = Math.max(1, ...months.map((m) => m.total))
  const n = months.length

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
        aria-label={`Sign-ins per month: ${months.map((mo) => `${monthLabel(mo, true)} ${mo.total}`).join(', ')}`}
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
      <p className="sr-only">
        {'Sign-ins by month: '}
        {months
          .map(
            (mo) =>
              `${monthLabel(mo, true)}: ${
                mo.total === 0
                  ? 'no sign-ins'
                  : `${mo.total} total, ${mo.local} local, ${mo.oidc} SSO, ${mo.unique_users} unique ${mo.unique_users === 1 ? 'user' : 'users'}`
              }`,
          )
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

// Mirrors store.RoleIsNetworkScoped: these two carry a network set, the
// other two are global and the server refuses the field for them.
const SCOPED_ROLES: ReadonlySet<Role> = new Set<Role>(['network_admin', 'network_viewer'])
const ALL_ROLES: Role[] = ['admin', 'viewer', 'network_admin', 'network_viewer']

// Only a local, scoped account has an editable scope: the server refuses the
// field for a global role, and a federated account's networks are re-derived
// from the IdP mapping on every login.
const scopeEditable = (u: UserAccount) => u.status !== 'deleted' && u.auth_source === 'local' && u.networks !== null

// A checkbox list rather than a multi-select: scope is a small set the
// operator must be able to read back at a glance, and <select multiple> is
// notoriously easy to clear by accident.
function NetworkPicker({
  all,
  value,
  disabled,
  onChange,
}: {
  all: string[]
  value: string[]
  disabled?: boolean
  onChange: (next: string[]) => void
}) {
  if (all.length === 0) {
    return <span className="hint">no networks exist yet — create one under Settings → Networks</span>
  }
  return (
    <div className="chips users-network-picker">
      {all.map((n) => (
        <label key={n} className="chip">
          <input
            type="checkbox"
            checked={value.includes(n)}
            disabled={disabled}
            onChange={(e) => onChange(e.target.checked ? [...value, n] : value.filter((v) => v !== n))}
          />
          {n}
        </label>
      ))}
    </div>
  )
}

type RoleFilter = '' | Role
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
  caps,
  canWrite,
  networks,
  currentUsername,
  onAuthError,
}: {
  caps: Caps
  canWrite: boolean
  // Every plane on the install: this panel is admin-only, so the list is
  // unfiltered and any of them may be assigned.
  networks: string[]
  currentUsername: string
  onAuthError: (err: unknown) => void
}) {
  useTimezone() // re-render fmtTime renders on UTC/local toggle
  const [data, setData] = useState<UsersResponse | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
  const [query, setQuery] = useState('') // raw input
  const [q, setQ] = useState('') // debounced, applied to the fetch
  const [role, setRole] = useState<RoleFilter>('')
  const [status, setStatus] = useState<StatusFilter>('')
  const [source, setSource] = useState<SourceFilter>('')
  const [offset, setOffset] = useState(0)
  const [refresh, setRefresh] = useState(0) // bumped after any mutation
  const [actionError, setActionError] = useState('') // row disable/delete failures
  const [createError, setCreateError] = useState('') // shown inside the dialog
  const createSummary = useErrorSummary(Boolean(createError))
  const [newUsername, setNewUsername] = useState('')
  const [newRole, setNewRole] = useState<Role>('viewer')
  // Only meaningful for the two scoped roles: the server requires at least
  // one network for them and rejects the field outright for the global ones.
  const [newNetworks, setNewNetworks] = useState<string[]>([])
  // Which row's scope is being edited, by user id.
  const [scopeEdit, setScopeEdit] = useState<{ id: string; networks: string[] } | null>(null)
  const [creating, setCreating] = useState(false)
  // The generated password lives in its own state so the 30 s poll can
  // never clear it — it is shown exactly once and cannot be recovered. The
  // kind picks the reveal copy: the same dialog serves create and reset.
  const [minted, setMinted] = useState<{ kind: 'created' | 'reset'; res: UserCreateResponse } | null>(null)
  const [copied, setCopied] = useState(false)
  // A reset mints a shown-once password per request, so overlapping resets
  // could reveal a password the second request already replaced — block
  // further resets while one is in flight.
  const [resetting, setResetting] = useState(false)
  const dialogRef = useRef<HTMLDialogElement>(null)
  // Names the create/reveal dialog from whichever <h2> is rendered — an
  // aria-label string would go stale when the content switches.
  const dialogTitleID = useId()
  const feedback = useSettingsMutation()
  const editingUser = scopeEdit ? data?.users.find((user) => user.id === scopeEdit.id) : undefined
  const loadedScope = editingUser
    ? { exists: true, id: editingUser.id, networks: orderedNetworks(editingUser.networks ?? []) }
    : null
  const currentScope = scopeEdit
    ? { exists: true, id: scopeEdit.id, networks: orderedNetworks(scopeEdit.networks) }
    : null
  const scopeGuard = useConcurrentSettingsDraft({
    id: `user-scope:${scopeEdit?.id ?? 'none'}`,
    label: editingUser ? `Network access for ${editingUser.username}` : 'User network access',
    loaded: loadedScope,
    current: currentScope,
    editing: scopeEdit !== null,
    discard: () => {
      setScopeEdit(null)
      setActionError('')
    },
    reload: (latest) => setScopeEdit(latest.exists ? { id: latest.id, networks: latest.networks } : null),
  })
  const discardCreate = () => {
    setNewUsername('')
    setNewRole('viewer')
    setNewNetworks([])
    setCreateError('')
    setMinted(null)
    setCopied(false)
    dialogRef.current?.close()
  }
  useSettingsDraft(
    'new-user',
    minted ? `One-time password for ${minted.res.username}` : 'New local user',
    minted !== null || newUsername !== '' || newRole !== 'viewer' || newNetworks.length > 0,
    discardCreate,
    minted
      ? 'This password is shown only once and cannot be recovered. Discarding it requires resetting the password again.'
      : undefined,
  )

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
    if (!canWrite) return
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
            setError(null)
          }
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          console.error('user settings request failed', err)
          setError(err)
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [canWrite, onAuthError, q, role, status, source, offset, refresh, retryKey])

  // Only a local, scoped account has an editable scope: the server refuses
  // the field for a global role, and a federated account's networks are
  // re-derived from the IdP mapping on every login.

  const saveScope = async (u: UserAccount, next: string[]) => {
    setActionError('')
    try {
      const currentServer = await scopeGuard.checkForConflict(async () => {
        const latest = await apiGet<UsersResponse>(`/api/v1/users?q=${encodeURIComponent(u.username)}&limit=100`)
        const account = latest.users.find((item) => item.id === u.id)
        return {
          exists: account !== undefined,
          id: u.id,
          networks: orderedNetworks(account?.networks ?? []),
        }
      })
      if (!currentServer) return
      await apiPut('/api/v1/users/' + encodeURIComponent(u.id), { networks: next })
      setScopeEdit(null)
      feedback.success(`Network access for ${u.username} saved.`)
      setRefresh((r) => r + 1)
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setActionError(message)
      feedback.error(`Network access for ${u.username} was not saved: ${message}`)
    }
  }

  const openCreate = () => {
    setCreateError('')
    setNewUsername('')
    setNewRole('viewer')
    setNewNetworks([])
    dialogRef.current?.showModal()
  }

  const create = async () => {
    if (!newUsername.trim()) return
    setCreating(true)
    setCreateError('')
    setCopied(false)
    try {
      // The server requires networks for a scoped role and refuses the
      // field for a global one, so send it exactly when it applies.
      const res = await apiPost<UserCreateResponse>('/api/v1/users', {
        username: newUsername.trim(),
        role: newRole,
        ...(SCOPED_ROLES.has(newRole) ? { networks: newNetworks } : {}),
      })
      setMinted({ kind: 'created', res })
      setNewUsername('')
      setNewRole('viewer')
      setNewNetworks([])
      setRefresh((r) => r + 1)
      feedback.success(`User ${res.username} created.`)
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setCreateError(message)
      createSummary.request()
      feedback.error(`User was not created: ${message}`)
    } finally {
      setCreating(false)
    }
  }

  const finishReveal = () => {
    setMinted(null)
    setCopied(false)
    dialogRef.current?.close()
  }

  const setDisabled = async (u: UserAccount, disabled: boolean) => {
    setActionError('')
    try {
      await apiPut('/api/v1/users/' + encodeURIComponent(u.id), { disabled })
      feedback.success(`User ${u.username} ${disabled ? 'disabled' : 'enabled'}.`)
      setRefresh((r) => r + 1)
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setActionError(message)
      feedback.error(`User ${u.username} was not changed: ${message}`)
    }
  }

  const remove = async (u: UserAccount) => {
    setActionError('')
    try {
      await apiDelete('/api/v1/users/' + encodeURIComponent(u.id))
      feedback.success(`User ${u.username} deleted.`)
      setRefresh((r) => r + 1)
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setActionError(message)
      feedback.error(`User ${u.username} was not deleted: ${message}`)
    }
  }

  const resetPassword = async (u: UserAccount) => {
    if (resetting) return
    setResetting(true)
    setActionError('')
    setCopied(false)
    try {
      const res = await apiPost<UserCreateResponse>('/api/v1/users/' + encodeURIComponent(u.id) + '/reset-password')
      setMinted({ kind: 'reset', res })
      feedback.success(`Password for ${u.username} reset.`)
      setRefresh((r) => r + 1)
      dialogRef.current?.showModal()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setActionError(message)
      feedback.error(`Password for ${u.username} was not reset: ${message}`)
    } finally {
      setResetting(false)
    }
  }

  const copyMinted = () => {
    if (!minted) return
    navigator.clipboard.writeText(minted.res.password).then(
      () => setCopied(true),
      () => setCopied(false),
    )
  }

  if (!canWrite) {
    return <RoleWall need="adminWrite" what="User accounts and sign-in activity" caps={caps} />
  }
  if (error && !data) {
    return (
      <SettingsPageError
        title="User accounts unavailable"
        subject="user accounts"
        error={error}
        onRetry={() => setRetryKey((key) => key + 1)}
      />
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
      {error !== null && (
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
          <button type="button" className="primary" onClick={openCreate}>
            Create user
          </button>
        </div>
        <p className="section-intro">
          Single sign-on accounts are provisioned automatically at first login. Deleted accounts stay listed with their
          last-known details as long as their sign-in history is retained. Sign-in counts start when this server first
          records logins.
        </p>
        {/* Native <dialog>: modal focus, Esc, and backdrop come from the
            platform, keeping the no-overlay-machinery rule intact. Esc is
            suppressed only while the one-time password is displayed. */}
        <dialog
          ref={dialogRef}
          className="users-dialog"
          aria-labelledby={dialogTitleID}
          onCancel={(e) => {
            if (minted) e.preventDefault()
            else {
              setNewUsername('')
              setNewRole('viewer')
              setNewNetworks([])
              setCreateError('')
            }
          }}
        >
          {minted ? (
            <>
              <h2 id={dialogTitleID}>{minted.kind === 'reset' ? 'Password reset' : 'User created'}</h2>
              <p className="section-intro">
                <strong>{minted.res.username}</strong> ({minted.res.role}). Copy the {minted.kind === 'reset' && 'new '}
                password now — it is shown only once and cannot be recovered.
                {minted.kind === 'reset' && ' All of their sessions have been signed out.'}
              </p>
              <div className="mono token-reveal">{minted.res.password}</div>
              <div className="users-dialog-foot">
                <button type="button" className="secondary-button" onClick={copyMinted}>
                  {copied ? 'Copied' : 'Copy'}
                </button>
                <button type="button" className="primary" onClick={finishReveal}>
                  Done
                </button>
              </div>
            </>
          ) : (
            <form
              onSubmit={(e) => {
                e.preventDefault()
                void create()
              }}
            >
              <h2 id={dialogTitleID}>Create local user</h2>
              <p className="section-intro">
                The password is generated and shown once — hand it to the user out-of-band. Federated users sign in via
                SSO instead.
              </p>
              <div className="config-form-grid">
                <label className="threshold-field">
                  <span className="eyebrow">Username</span>
                  <span className="threshold-input">
                    <input
                      type="text"
                      value={newUsername}
                      disabled={creating}
                      placeholder="username"
                      aria-describedby={createSummary.describedby}
                      onChange={(e) => setNewUsername(e.target.value)}
                    />
                  </span>
                </label>
                <label className="threshold-field">
                  <span className="eyebrow">Role</span>
                  <span className="threshold-input">
                    <select
                      value={newRole}
                      disabled={creating}
                      aria-describedby={createSummary.describedby}
                      onChange={(e) => setNewRole(e.target.value as Role)}
                    >
                      {ALL_ROLES.map((r) => (
                        <option key={r} value={r}>
                          {roleLabel(r)}
                        </option>
                      ))}
                    </select>
                    <span className="hint">
                      {SCOPED_ROLES.has(newRole)
                        ? 'limited to the networks below; sees nothing outside them'
                        : 'sees every network'}
                    </span>
                  </span>
                </label>
              </div>
              {/* The role cannot be changed afterwards — only the scope of a
                scoped account can — so this choice is the durable one. */}
              {SCOPED_ROLES.has(newRole) && (
                <div
                  className="threshold-field"
                  role="group"
                  aria-label="Networks"
                  aria-describedby={createSummary.describedby}
                >
                  <span className="eyebrow">Networks</span>
                  <NetworkPicker all={networks} value={newNetworks} disabled={creating} onChange={setNewNetworks} />
                </div>
              )}
              {createError && (
                <ul className="error threshold-errors" id={createSummary.id} ref={createSummary.ref} tabIndex={-1}>
                  <li>{createError}</li>
                </ul>
              )}
              <div className="users-dialog-foot">
                <button type="button" className="linklike" disabled={creating} onClick={discardCreate}>
                  Cancel
                </button>
                <button
                  type="submit"
                  className="primary"
                  disabled={creating || !newUsername.trim() || (SCOPED_ROLES.has(newRole) && newNetworks.length === 0)}
                >
                  {creating ? 'Creating…' : 'Create user'}
                </button>
              </div>
            </form>
          )}
        </dialog>
        {actionError && (
          <div className="inline-alert" role="alert">
            {actionError}
          </div>
        )}
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
            options={[{ value: '', label: 'All roles' }, ...ALL_ROLES.map((r) => ({ value: r, label: roleLabel(r) }))]}
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
                    <th>Networks</th>
                    <th>Source</th>
                    <th>Status</th>
                    <th>Sign-ins</th>
                    <th>Last sign-in</th>
                    <th>Created</th>
                    <th className="actions-col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {data.users.map((u) => (
                    <Fragment key={u.id}>
                      <tr>
                        <td data-label="Username" className="mono">
                          {u.username}
                        </td>
                        {/* Was `role === 'admin' ? 'Admin' : 'Viewer'`, which
                        labelled every scoped role "Viewer" — including a
                        network admin. */}
                        <td data-label="Role">{roleLabel(u.role)}</td>
                        <td data-label="Networks">
                          {/* A deleted identity also reports null, because
                            its scope rows are gone — so null alone cannot
                            mean "global". Saying "all" for a deleted
                            network_admin would claim it once had access it
                            never had, in the one view that exists to answer
                            that question. */}
                          {u.networks === null && SCOPED_ROLES.has(u.role) ? (
                            <span className="hint">unknown</span>
                          ) : u.networks === null ? (
                            <span className="hint">all</span>
                          ) : u.networks.length === 0 ? (
                            // Assignable but unassigned: this account can see
                            // nothing until an operator gives it a plane.
                            <span className="status-text-down">none</span>
                          ) : (
                            u.networks.join(', ')
                          )}
                        </td>
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
                        <td data-label="Actions" className="users-actions">
                          {u.status !== 'deleted' && (
                            <>
                              {scopeEditable(u) && (
                                <button
                                  type="button"
                                  className="secondary-button"
                                  aria-expanded={scopeEdit?.id === u.id}
                                  onClick={() => {
                                    if (scopeEdit?.id === u.id && scopeGuard.dirty) {
                                      feedback.confirm({
                                        action: 'Discard changes',
                                        resource: `Network access for ${u.username}`,
                                        consequence: 'This closes the editor and discards the local network selection.',
                                        confirmLabel: 'Discard',
                                        cancelLabel: 'Stay',
                                        onConfirm: () => setScopeEdit(null),
                                      })
                                      return
                                    }
                                    setScopeEdit(
                                      scopeEdit?.id === u.id ? null : { id: u.id, networks: [...(u.networks ?? [])] },
                                    )
                                  }}
                                >
                                  {scopeEdit?.id === u.id ? 'Close' : 'Networks'}
                                </button>
                              )}
                              {/* Hidden (not disabled) for SSO accounts: they
                                have no password to reset, per the schema. */}
                              {u.auth_source !== 'oidc' && (
                                <ConfirmButton
                                  label="Reset password"
                                  resource={`Local user ${u.username}`}
                                  consequence="This replaces their password and signs out their active sessions."
                                  disabled={u.username === currentUsername || resetting}
                                  title={
                                    u.username === currentUsername
                                      ? 'Change your own password from the user menu'
                                      : 'Generates a new password shown once and signs out all of their sessions'
                                  }
                                  onConfirm={() => resetPassword(u)}
                                />
                              )}
                              <ConfirmButton
                                label={u.status === 'disabled' ? 'Enable' : 'Disable'}
                                resource={`User ${u.username}`}
                                consequence={
                                  u.status === 'disabled'
                                    ? 'This restores the user’s ability to sign in.'
                                    : 'This blocks sign-in and ends the user’s active sessions.'
                                }
                                disabled={u.username === currentUsername}
                                title={
                                  u.username === currentUsername
                                    ? 'You cannot disable your own account'
                                    : u.auth_source === 'oidc'
                                      ? 'Blocks sign-in even if the IdP still authorizes this user'
                                      : undefined
                                }
                                onConfirm={() => setDisabled(u, u.status !== 'disabled')}
                              />
                              <ConfirmButton
                                label="Delete"
                                resource={`User ${u.username}`}
                                consequence={
                                  u.auth_source === 'oidc'
                                    ? 'This deletes the account. A later SSO sign-in may provision it again.'
                                    : 'This permanently deletes the local account and ends its sessions.'
                                }
                                disabled={u.username === currentUsername}
                                title={
                                  u.username === currentUsername
                                    ? 'You cannot delete your own account'
                                    : u.auth_source === 'oidc'
                                      ? 'Deleting does not revoke IdP access — a still-authorized user is re-provisioned on next SSO login. Disable to revoke.'
                                      : undefined
                                }
                                onConfirm={() => remove(u)}
                              />
                            </>
                          )}
                        </td>
                      </tr>
                      {scopeEdit?.id === u.id && (
                        <tr className="config-edit-row">
                          <td colSpan={9}>
                            <div className="config-form">
                              <h3 className="eyebrow">Networks · {u.username}</h3>
                              <NetworkPicker
                                all={networks}
                                value={scopeEdit.networks}
                                onChange={(next) => setScopeEdit({ id: u.id, networks: next })}
                              />
                              <div className="threshold-foot">
                                <span className="hint">
                                  A scoped account needs at least one network; removing the last one is refused.
                                </span>
                                <button
                                  className="primary"
                                  type="button"
                                  disabled={scopeEdit.networks.length === 0 || !scopeGuard.dirty}
                                  onClick={() => void saveScope(u, scopeEdit.networks)}
                                >
                                  Save
                                </button>
                              </div>
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
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
