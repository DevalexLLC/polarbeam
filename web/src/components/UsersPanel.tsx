import { useEffect, useRef, useState } from 'react'
import { apiDelete, apiGet, apiPost, apiPut } from '../api'
import type { Caps } from '../caps'
import { roleLabel } from '../caps'
import { useConcurrentSettingsDraft, useSettingsMutation } from '../settingsMutation'
import type { Role, UserAccount, UserCreateResponse, UsersResponse } from '../types'
import { usePolledResource } from '../usePolledResource'
import { ALL_ROLES } from '../userRoles'
import LoginBars from './LoginBars'
import RoleWall from './RoleWall'
import SettingsPageError from './SettingsPageError'
import UserCreateDialog, { type MintedSecret } from './UserCreateDialog'
import UsersTable from './UsersTable'

function orderedNetworks(values: string[]): string[] {
  // ES2023 toSorted is outside this project's browser target. The spread
  // is a fresh array, so this does not mutate React or API state.
  // oxlint-disable-next-line unicorn/no-array-sort
  return [...values].sort()
}

const PAGE_SIZE = 50

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
  const [query, setQuery] = useState('') // raw input
  const [q, setQ] = useState('') // debounced, applied to the fetch
  const [role, setRole] = useState<RoleFilter>('')
  const [status, setStatus] = useState<StatusFilter>('')
  const [source, setSource] = useState<SourceFilter>('')
  const [offset, setOffset] = useState(0)
  const [actionError, setActionError] = useState('') // row disable/delete failures

  const listParams = new URLSearchParams()
  if (q) listParams.set('q', q)
  if (role) listParams.set('role', role)
  if (status) listParams.set('status', status)
  if (source) listParams.set('source', source)
  listParams.set('limit', String(PAGE_SIZE))
  if (offset > 0) listParams.set('offset', String(offset))
  // Like the token list, this GET is admin-only (usernames, roles, and
  // sign-in history), so viewers get a static explanation instead of a
  // doomed fetch.
  const { data, error, reload } = usePolledResource<UsersResponse>('/api/v1/users?' + listParams.toString(), {
    enabled: canWrite,
    onAuthError,
    logLabel: 'user settings',
  })

  // Which row's scope is being edited, by user id.
  const [scopeEdit, setScopeEdit] = useState<{ id: string; networks: string[] } | null>(null)
  // The generated password lives in its own state so the 30 s poll can
  // never clear it — it is shown exactly once and cannot be recovered. The
  // kind picks the reveal copy: the same dialog serves create and reset,
  // which is why the dialog element itself is shared through this ref.
  const [minted, setMinted] = useState<MintedSecret | null>(null)
  // A reset mints a shown-once password per request, so overlapping resets
  // could reveal a password the second request already replaced — block
  // further resets while one is in flight.
  const [resetting, setResetting] = useState(false)
  const dialogRef = useRef<HTMLDialogElement>(null)
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

  const toggleScope = (u: UserAccount) => {
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
    setScopeEdit(scopeEdit?.id === u.id ? null : { id: u.id, networks: [...(u.networks ?? [])] })
  }

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
      void reload()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setActionError(message)
      feedback.error(`Network access for ${u.username} was not saved: ${message}`)
    }
  }

  const setDisabled = async (u: UserAccount, disabled: boolean) => {
    setActionError('')
    try {
      await apiPut('/api/v1/users/' + encodeURIComponent(u.id), { disabled })
      feedback.success(`User ${u.username} ${disabled ? 'disabled' : 'enabled'}.`)
      void reload()
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
      void reload()
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
    try {
      const res = await apiPost<UserCreateResponse>('/api/v1/users/' + encodeURIComponent(u.id) + '/reset-password')
      setMinted({ kind: 'reset', res })
      feedback.success(`Password for ${u.username} reset.`)
      void reload()
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

  if (!canWrite) {
    return <RoleWall need="adminWrite" what="User accounts and sign-in activity" caps={caps} />
  }
  if (error && !data) {
    return (
      <SettingsPageError
        title="User accounts unavailable"
        subject="user accounts"
        error={error}
        onRetry={() => void reload()}
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
          <UserCreateDialog
            dialogRef={dialogRef}
            networks={networks}
            minted={minted}
            onMintedChange={setMinted}
            onCreated={() => void reload()}
            onAuthError={onAuthError}
          />
        </div>
        <p className="section-intro">
          Single sign-on accounts are provisioned automatically at first login. Deleted accounts stay listed with their
          last-known details as long as their sign-in history is retained. Sign-in counts start when this server first
          records logins.
        </p>
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
            <UsersTable
              users={data.users}
              networks={networks}
              currentUsername={currentUsername}
              resetting={resetting}
              scopeEdit={scopeEdit}
              scopeDirty={scopeGuard.dirty}
              onToggleScope={toggleScope}
              onScopeNetworksChange={(u, next) => setScopeEdit({ id: u.id, networks: next })}
              onSaveScope={(u, next) => void saveScope(u, next)}
              onResetPassword={(u) => void resetPassword(u)}
              onSetDisabled={(u, disabled) => void setDisabled(u, disabled)}
              onRemove={(u) => void remove(u)}
            />
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
