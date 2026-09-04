import { useEffect, useId, useRef, useState, type FormEvent } from 'react'
import { ApiError, apiPut } from '../api'
import { useErrorSummary } from '../formErrors'

// The CLI and server enforce this; checking client-side just saves a round
// trip — the server remains the authority.
const MIN_PASSWORD_LEN = 8

// PasswordField mirrors the login form's show/hide input.
function PasswordField({
  label,
  value,
  autoComplete,
  disabled,
  invalid,
  describedby,
  onChange,
}: {
  label: string
  value: string
  autoComplete: string
  disabled: boolean
  invalid?: boolean
  describedby?: string
  onChange: (v: string) => void
}) {
  const [show, setShow] = useState(false)
  return (
    <label className="threshold-field">
      <span className="label">{label}</span>
      <span className="threshold-input password-field">
        <input
          type={show ? 'text' : 'password'}
          autoComplete={autoComplete}
          value={value}
          disabled={disabled}
          aria-invalid={invalid || undefined}
          aria-describedby={describedby}
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
  // Named by whichever <h2> is rendered — "Change password" becomes
  // "Password changed" on success, which a static label would miss.
  const titleID = useId()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  // Each failure names the field it belongs to (null = the whole form) so
  // only that field is marked invalid.
  const [error, setError] = useState<{ field: 'current' | 'next' | 'confirm' | null; text: string } | null>(null)
  const summary = useErrorSummary(Boolean(error))
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
      setError({ field: 'next', text: `New password must be at least ${MIN_PASSWORD_LEN} characters.` })
      summary.request()
      return
    }
    if (next !== confirm) {
      setError({ field: 'confirm', text: 'New passwords do not match.' })
      summary.request()
      return
    }
    setBusy(true)
    setError(null)
    try {
      await apiPut('/api/v1/auth/password', { current_password: current, new_password: next })
      setDone(true)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        // A real 401 means the session died server-side: back to login.
        onAuthError(err)
      } else if (err instanceof ApiError && err.status === 429) {
        setError({ field: null, text: 'Too many attempts — wait a minute and try again.' })
        summary.request()
      } else {
        setError({ field: null, text: err instanceof Error ? err.message : String(err) })
        summary.request()
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <dialog
      ref={dialogRef}
      className="users-dialog password-dialog"
      aria-labelledby={titleID}
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
          <h2 id={titleID}>Password changed</h2>
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
          <h2 id={titleID}>Change password</h2>
          <p className="section-intro">Changing your password signs out your other sessions. This one stays active.</p>
          <div className="config-form-grid">
            <PasswordField
              label="Current password"
              value={current}
              autoComplete="current-password"
              disabled={busy}
              invalid={error?.field === 'current'}
              describedby={error && (error.field === 'current' || error.field === null) ? summary.id : undefined}
              onChange={setCurrent}
            />
            <PasswordField
              label="New password"
              value={next}
              autoComplete="new-password"
              disabled={busy}
              invalid={error?.field === 'next'}
              describedby={error && (error.field === 'next' || error.field === null) ? summary.id : undefined}
              onChange={setNext}
            />
            <PasswordField
              label="Confirm new password"
              value={confirm}
              autoComplete="new-password"
              disabled={busy}
              invalid={error?.field === 'confirm'}
              describedby={error && (error.field === 'confirm' || error.field === null) ? summary.id : undefined}
              onChange={setConfirm}
            />
          </div>
          {error && (
            <ul className="error threshold-errors" id={summary.id} ref={summary.ref} tabIndex={-1}>
              <li>{error.text}</li>
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
