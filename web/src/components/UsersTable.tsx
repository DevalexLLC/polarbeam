import { Fragment } from 'react'
import { roleLabel } from '../caps'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
import type { UserAccount } from '../types'
import { SCOPED_ROLES, scopeEditable } from '../userRoles'
import ConfirmButton from './ConfirmButton'
import NetworkPicker from './NetworkPicker'

// The accounts table with its per-row actions and the inline network-scope
// editor. Purely presentational: the mutations, the scope-edit state, and
// its concurrent-draft guard all live in UsersPanel and arrive as props.
export default function UsersTable({
  users,
  networks,
  currentUsername,
  resetting,
  scopeEdit,
  scopeDirty,
  onToggleScope,
  onScopeNetworksChange,
  onSaveScope,
  onResetPassword,
  onSetDisabled,
  onRemove,
}: {
  users: UserAccount[]
  networks: string[]
  currentUsername: string
  resetting: boolean
  scopeEdit: { id: string; networks: string[] } | null
  scopeDirty: boolean
  onToggleScope: (u: UserAccount) => void
  onScopeNetworksChange: (u: UserAccount, next: string[]) => void
  onSaveScope: (u: UserAccount, next: string[]) => void
  onResetPassword: (u: UserAccount) => void
  onSetDisabled: (u: UserAccount, disabled: boolean) => void
  onRemove: (u: UserAccount) => void
}) {
  useTimezone() // re-render fmtTime renders on UTC/local toggle
  return (
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
          {users.map((u) => (
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
                          onClick={() => onToggleScope(u)}
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
                          onConfirm={() => onResetPassword(u)}
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
                        onConfirm={() => onSetDisabled(u, u.status !== 'disabled')}
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
                        onConfirm={() => onRemove(u)}
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
                        onChange={(next) => onScopeNetworksChange(u, next)}
                      />
                      <div className="threshold-foot">
                        <span className="hint">
                          A scoped account needs at least one network; removing the last one is refused.
                        </span>
                        <button
                          className="primary"
                          type="button"
                          disabled={scopeEdit.networks.length === 0 || !scopeDirty}
                          onClick={() => onSaveScope(u, scopeEdit.networks)}
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
  )
}
