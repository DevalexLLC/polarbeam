import { useEffect, useRef, useState, type FormEvent } from 'react'
import { ApiError, apiPut } from '../api'

// The CLI and server enforce this; checking client-side just saves a round
// trip — the server remains the authority.
const MIN_PASSWORD_LEN = 8

// PasswordField mirrors the login form's show/hide input.
function PasswordField({
  label,
  value,
  autoComplete,
  disabled,
  onChange,
}: {
  label: string
  value: string
  autoComplete: string
  disabled: boolean
  onChange: (v: string) => void
}) {
  const [show, setShow] = useState(false)
  return (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input password-field">
        <input
          type={show ? 'text' : 'password'}
          autoComplete={autoComplete}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
        <button type="button" className="password-toggle" aria-pressed={show} onClick={() => setShow(!show)}>
          {show ? 'Hide' : 'Show'}
        </button>
      </span>
    </label>
  )
}

// Self-service password change for local accounts, opened from the user
// menu. Mounted only while open; the native <dialog> supplies modal focus,
// Esc, and backdrop (the UsersPanel no-overlay-machinery rule).
export default function ChangePasswordDialog({
  onClose,
  onAuthError,
}: {
  onClose: () => void
  onAuthError: (err: unknown) => void
}) {
  const dialogRef = useRef<HTMLDialogElement>(null)
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState(false)

  useEffect(() => {
    dialogRef.current?.showModal()
  }, [])

  async function submit(e: FormEvent) {
    e.preventDefault()
    // Mirror the server's checks so a typo doesn't burn a rate-limited
    // verification attempt. Spread counts code points, matching the
    // server's rune count (.length counts UTF-16 units).
    if ([...next].length < MIN_PASSWORD_LEN) {
      setError(`New password must be at least ${MIN_PASSWORD_LEN} characters.`)
      return
    }
    if (next !== confirm) {
      setError('New passwords do not match.')
      return
    }
    setBusy(true)
    setError('')
    try {
      await apiPut('/api/v1/auth/password', { current_password: current, new_password: next })
      setDone(true)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        // A real 401 means the session died server-side: back to login.
        onAuthError(err)
      } else if (err instanceof ApiError && err.status === 429) {
        setError('Too many attempts — wait a minute and try again.')
      } else {
        setError(err instanceof Error ? err.message : String(err))
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <dialog
      ref={dialogRef}
      className="users-dialog password-dialog"
      aria-label="Change password"
      onClose={onClose}
      onCancel={(e) => {
        // Esc must not dismiss the dialog while the request is in flight —
        // the change would complete invisibly (and sign out other sessions)
        // with neither confirmation nor error.
        if (busy) e.preventDefault()
      }}
    >
      {done ? (
        <>
          <h2>Password changed</h2>
          <p className="section-intro">
            Use the new password next time you sign in. Your other sessions were signed out; this one stays active.
          </p>
          <div className="users-dialog-foot">
            <button type="button" className="primary" onClick={() => dialogRef.current?.close()}>
              Done
            </button>
          </div>
        </>
      ) : (
        <form onSubmit={submit} aria-busy={busy}>
          <h2>Change password</h2>
          <p className="section-intro">Changing your password signs out your other sessions. This one stays active.</p>
          <div className="config-form-grid">
            <PasswordField
              label="Current password"
              value={current}
              autoComplete="current-password"
              disabled={busy}
              onChange={setCurrent}
            />
            <PasswordField
              label="New password"
              value={next}
              autoComplete="new-password"
              disabled={busy}
              onChange={setNext}
            />
            <PasswordField
              label="Confirm new password"
              value={confirm}
              autoComplete="new-password"
              disabled={busy}
              onChange={setConfirm}
            />
          </div>
          {error && (
            <ul className="error threshold-errors">
              <li>{error}</li>
            </ul>
          )}
          <div className="users-dialog-foot">
            <button type="button" className="linklike" disabled={busy} onClick={() => dialogRef.current?.close()}>
              Cancel
            </button>
            <button type="submit" className="primary" disabled={busy || !current || !next || !confirm}>
              {busy ? 'Changing…' : 'Change password'}
            </button>
          </div>
        </form>
      )}
    </dialog>
  )
}
