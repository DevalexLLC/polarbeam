import { type RefObject, useId, useState } from 'react'
import { apiPost } from '../api'
import { roleLabel } from '../caps'
import { useErrorSummary } from '../formErrors'
import { useSettingsDraft, useSettingsMutation } from '../settingsMutation'
import type { Role, UserCreateResponse } from '../types'
import { ALL_ROLES, SCOPED_ROLES } from '../userRoles'
import NetworkPicker from './NetworkPicker'

export type MintedSecret = { kind: 'created' | 'reset'; res: UserCreateResponse }

// The "Create user" trigger plus the shared <dialog> that serves both the
// create form and the one-time-password reveal. The dialog and the minted
// secret stay reachable from the panel (via dialogRef / onMintedChange)
// because a row's password reset mints into the same reveal.
export default function UserCreateDialog({
  dialogRef,
  networks,
  minted,
  onMintedChange,
  onCreated,
  onAuthError,
}: {
  dialogRef: RefObject<HTMLDialogElement | null>
  // Every plane on the install: this panel is admin-only, so the list is
  // unfiltered and any of them may be assigned.
  networks: string[]
  minted: MintedSecret | null
  onMintedChange: (minted: MintedSecret | null) => void
  onCreated: () => void
  onAuthError: (err: unknown) => void
}) {
  const [createError, setCreateError] = useState('') // shown inside the dialog
  const {
    request: createSummaryRequest,
    describedby: createSummaryDescribedby,
    id: createSummaryId,
    ref: createSummaryRef,
  } = useErrorSummary(Boolean(createError))
  const [newUsername, setNewUsername] = useState('')
  const [newRole, setNewRole] = useState<Role>('viewer')
  // Only meaningful for the two scoped roles: the server requires at least
  // one network for them and rejects the field outright for the global ones.
  const [newNetworks, setNewNetworks] = useState<string[]>([])
  const [creating, setCreating] = useState(false)
  const [copiedSecret, setCopiedSecret] = useState<MintedSecret | null>(null)
  const copied = minted !== null && copiedSecret === minted
  // Names the create/reveal dialog from whichever <h2> is rendered — an
  // aria-label string would go stale when the content switches.
  const dialogTitleID = useId()
  const feedback = useSettingsMutation()

  // Every path that replaces or clears the minted password (a create, a
  // row's reset, Done, discard) must also reset the Copy button's state.
  if (copiedSecret !== null && copiedSecret !== minted) setCopiedSecret(null)

  const discardCreate = () => {
    setNewUsername('')
    setNewRole('viewer')
    setNewNetworks([])
    setCreateError('')
    onMintedChange(null)
    setCopiedSecret(null)
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
    setCopiedSecret(null)
    try {
      // The server requires networks for a scoped role and refuses the
      // field for a global one, so send it exactly when it applies.
      const res = await apiPost<UserCreateResponse>('/api/v1/users', {
        username: newUsername.trim(),
        role: newRole,
        ...(SCOPED_ROLES.has(newRole) ? { networks: newNetworks } : {}),
      })
      onMintedChange({ kind: 'created', res })
      setNewUsername('')
      setNewRole('viewer')
      setNewNetworks([])
      onCreated()
      feedback.success(`User ${res.username} created.`)
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      setCreateError(message)
      createSummaryRequest()
      feedback.error(`User was not created: ${message}`)
    } finally {
      setCreating(false)
    }
  }

  const finishReveal = () => {
    onMintedChange(null)
    setCopiedSecret(null)
    dialogRef.current?.close()
  }

  const copyMinted = () => {
    if (!minted) return
    navigator.clipboard.writeText(minted.res.password).then(
      () => setCopiedSecret(minted),
      () => setCopiedSecret(null),
    )
  }

  return (
    <>
      <button type="button" className="primary" onClick={openCreate}>
        Create user
      </button>
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
                <span className="label">Username</span>
                <span className="threshold-input">
                  <input
                    type="text"
                    value={newUsername}
                    disabled={creating}
                    placeholder="username"
                    aria-describedby={createSummaryDescribedby}
                    onChange={(e) => setNewUsername(e.target.value)}
                  />
                </span>
              </label>
              <label className="threshold-field">
                <span className="label">Role</span>
                <span className="threshold-input">
                  <select
                    value={newRole}
                    disabled={creating}
                    aria-describedby={createSummaryDescribedby}
                    onChange={(e) => setNewRole(e.target.value as Role)}
                  >
                    {ALL_ROLES.map((r) => (
                      <option key={r} value={r}>
                        {roleLabel(r)}
                      </option>
                    ))}
                  </select>
                </span>
                <span className="hint field-note">
                  {SCOPED_ROLES.has(newRole)
                    ? 'limited to the networks below; sees nothing outside them'
                    : 'sees every network'}
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
                aria-describedby={createSummaryDescribedby}
              >
                <span className="label">Networks</span>
                <NetworkPicker all={networks} value={newNetworks} disabled={creating} onChange={setNewNetworks} />
              </div>
            )}
            {createError && (
              <ul className="error threshold-errors" id={createSummaryId} ref={createSummaryRef} tabIndex={-1}>
                <li>{createError}</li>
              </ul>
            )}
            <div className="users-dialog-foot">
              <button type="button" className="secondary-button" disabled={creating} onClick={discardCreate}>
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
    </>
  )
}
