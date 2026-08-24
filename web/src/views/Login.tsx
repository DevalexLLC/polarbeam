import { useEffect, useState, type FormEvent } from 'react'
import { ApiError, apiPost } from '../api'
import LogoMark from '../components/LogoMark'
import type { LoginResponse } from '../types'

// The OIDC start/callback endpoints report failures as short codes in the
// URL hash (never IdP error strings); the detail is in the server log.
const SSO_ERRORS: Record<string, string> = {
  provider:
    'Single sign-on failed: the identity provider could not be reached or returned an error. Details are in the server log.',
  config: 'Single sign-on is not fully configured. Sign in with a local account and check Settings → Authentication.',
  state: 'Single sign-on was interrupted — the sign-in attempt expired or did not match. Try again.',
  claims:
    'Single sign-on succeeded, but the identity token was missing required information. An administrator should check the claim mapping in Settings → Authentication.',
  disabled: 'This account has been disabled. Contact an administrator.',
  internal: 'Single sign-on failed because of a server error. Details are in the server log.',
}

export default function Login({ sso, onLogin }: { sso: boolean; onLogin: (res: LoginResponse) => void }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [showPassword, setShowPassword] = useState(false)

  // Surface a callback failure carried in the hash, then clean the URL so
  // a reload or bookmark does not replay the stale error.
  useEffect(() => {
    const consumeSSOError = () => {
      const m = /^#\/sso-error=([a-z-]+)$/.exec(location.hash)
      if (!m) return
      setError(SSO_ERRORS[m[1]] ?? 'Single sign-on failed. Details are in the server log.')
      history.replaceState(null, '', location.pathname + location.search)
    }
    consumeSSOError()
    window.addEventListener('hashchange', consumeSSOError)
    return () => window.removeEventListener('hashchange', consumeSSOError)
  }, [])

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const res = await apiPost<LoginResponse>('/api/v1/auth/login', { username, password })
      onLogin(res)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setError('Invalid username or password.')
      } else if (err instanceof ApiError && err.status === 429) {
        setError('Too many attempts — wait a minute and try again.')
      } else {
        console.error('login request failed', err)
        setError('Sign-in is temporarily unavailable. Try again.')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <section className="login-context" aria-label="Product introduction">
        <LogoMark className="logo-mark login-context-mark" />
        <h1>See the network clearly.</h1>
        <p>
          Monitor inter-site connectivity, correlate incidents, and investigate directional performance from one control
          plane.
        </p>
        <div className="login-signals" aria-hidden="true">
          <span />
          <span />
          <span />
          <span />
        </div>
      </section>
      <form className="login-card" onSubmit={submit} aria-busy={busy}>
        <div className="login-mark">
          <LogoMark className="logo-mark logo-mark-login" />
          <h1>PolarBEAM</h1>
        </div>
        <p className="login-sub">Sign in to the PolarBEAM control plane.</p>
        <label className="eyebrow">
          Username
          {/* Sign-in is this page's only purpose and this is its first field,
              so focusing it costs no orientation and saves every operator a
              keystroke. */}
          {/* oxlint-disable-next-line jsx-a11y/no-autofocus */}
          <input autoFocus autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} />
        </label>
        <label className="eyebrow">
          Password
          <span className="password-field">
            <input
              type={showPassword ? 'text' : 'password'}
              autoComplete="current-password"
              value={password}
              aria-invalid={Boolean(error)}
              aria-describedby={error ? 'login-error' : undefined}
              onChange={(e) => setPassword(e.target.value)}
            />
            <button
              type="button"
              className="password-toggle"
              aria-pressed={showPassword}
              onClick={() => setShowPassword(!showPassword)}
            >
              {showPassword ? 'Hide' : 'Show'}
            </button>
          </span>
        </label>
        {error && (
          <p className="error login-error" id="login-error" role="alert">
            {error}
          </p>
        )}
        <button type="submit" disabled={busy || !username || !password}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
        {sso && (
          <>
            <div className="login-divider" aria-hidden="true">
              or
            </div>
            {/* Full navigation on purpose: the server 302s to the IdP, and a
                fetch would be blocked by CSP and could not follow it. */}
            <button
              type="button"
              className="sso-button"
              onClick={() => window.location.assign('/api/v1/auth/oidc/start')}
            >
              Continue with single sign-on
            </button>
          </>
        )}
        {/* Static on purpose: this screen is unauthenticated, so it carries
            no server version. The About page has that, behind a session. */}
        <p className="login-legal">© 2026 Devalex LLC · AGPL-3.0-only</p>
      </form>
    </div>
  )
}
