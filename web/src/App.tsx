import { useCallback, useEffect, useState } from 'react'
import { ApiError, apiGet, apiPost, setCsrfToken } from './api'
import type { AuthProviders, LoginResponse, UIBanner, User } from './types'
import BannerFrame from './components/BannerFrame'
import ChangePasswordDialog from './components/ChangePasswordDialog'
import LogoMark from './components/LogoMark'
import ThemeToggle from './components/ThemeToggle'
import TimezoneToggle from './components/TimezoneToggle'
import About from './views/About'
import Agents from './views/Agents'
import Login from './views/Login'
import Overview from './views/Overview'
import Outages from './views/Outages'
import PairDetail from './views/PairDetail'
import Paths from './views/Paths'
import Settings from './views/Settings'
import TargetDetail from './views/TargetDetail'
import Targets from './views/Targets'

// Hash routing stays dependency-free and preserves the original route names
// as aliases, so bookmarks survive the information-architecture cleanup.
export type SettingsTab =
  | 'thresholds'
  | 'sites'
  | 'networks'
  | 'targets'
  | 'meshes'
  | 'probes'
  | 'enrollment'
  | 'users'
  | 'authentication'
  | 'banner'

const SETTINGS_TABS: SettingsTab[] = [
  'thresholds',
  'sites',
  'networks',
  'targets',
  'meshes',
  'probes',
  'enrollment',
  'users',
  'authentication',
  'banner',
]

type Route =
  | { view: 'overview' }
  | { view: 'pair'; a: string; b: string }
  | { view: 'target'; id: string }
  | { view: 'targets' }
  | { view: 'incidents' }
  | { view: 'routes' }
  | { view: 'agents'; agent: string | null }
  | { view: 'about' }
  | { view: 'settings'; tab: SettingsTab }

// A malformed percent-escape in a hand-edited bookmark must not blank the
// app; fall back to the raw segment so PairDetail shows a loud not-found.
function decodeSegment(s: string): string {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

function parseHash(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/')
  if (parts[0] === 'pair' && parts[1] && parts[2]) {
    return { view: 'pair', a: decodeSegment(parts[1]), b: decodeSegment(parts[2]) }
  }
  if (parts[0] === 'targets') return { view: 'targets' }
  // #/target/<id> is the per-target drill-down; a bad id shows the view's
  // own loud not-found rather than silently landing on the overview.
  if (parts[0] === 'target' && parts[1]) {
    return { view: 'target', id: decodeSegment(parts[1]) }
  }
  if (parts[0] === 'incidents' || parts[0] === 'outages') return { view: 'incidents' }
  if (parts[0] === 'routes' || parts[0] === 'paths') return { view: 'routes' }
  // #/agents/<id> deep-links to that agent's expanded detail; the hash is
  // the single source of truth for which row is open (see Agents.tsx).
  if (parts[0] === 'agents') return { view: 'agents', agent: parts[1] ? decodeSegment(parts[1]) : null }
  if (parts[0] === 'about') return { view: 'about' }
  if (parts[0] === 'settings') {
    // #/settings/<tab>; unknown or absent tabs land on thresholds so the
    // plain #/settings bookmark (and the user-menu link) keep working.
    const tab = SETTINGS_TABS.find((t) => t === parts[1]) ?? 'thresholds'
    return { view: 'settings', tab }
  }
  // #/connectivity and #/sightlines land here too: the map/matrix switch
  // lives on the Overview now, so those bookmarks alias to it.
  return { view: 'overview' }
}

const NAV: Array<{ href: string; label: string; isActive: (r: Route) => boolean }> = [
  // Pair detail is a drill-down reached from other views, so it keeps
  // Overview lit; target detail lights Targets, its browseable index.
  { href: '#/', label: 'Overview', isActive: (r) => r.view === 'overview' || r.view === 'pair' },
  { href: '#/incidents', label: 'Incidents', isActive: (r) => r.view === 'incidents' },
  { href: '#/routes', label: 'Routes', isActive: (r) => r.view === 'routes' },
  { href: '#/targets', label: 'Targets', isActive: (r) => r.view === 'targets' || r.view === 'target' },
  { href: '#/agents', label: 'Agents', isActive: (r) => r.view === 'agents' },
]

export default function App() {
  const [booted, setBooted] = useState(false)
  const [user, setUser] = useState<User | null>(null)
  const [serverVersion, setServerVersion] = useState('')
  const [sso, setSso] = useState(false)
  const [banner, setBanner] = useState<UIBanner | null>(null)
  const [changingPassword, setChangingPassword] = useState(false)
  const [route, setRoute] = useState<Route>(() => parseHash(location.hash))

  useEffect(() => {
    const onHash = () => setRoute(parseHash(location.hash))
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  // Restore an existing session (and its CSRF token) on boot.
  useEffect(() => {
    apiGet<LoginResponse>('/api/v1/auth/me')
      .then((res) => {
        setCsrfToken(res.csrf_token)
        setUser(res.user)
        setServerVersion(res.version)
      })
      .catch(() => setUser(null))
      .finally(() => setBooted(true))
  }, [])

  // Learn whether to offer single sign-on every time the login screen is
  // entered (boot without a session, and again after logout), so an admin
  // toggling OIDC converges without a reload. A providers failure only
  // hides the SSO button — local login must never depend on it.
  useEffect(() => {
    if (!booted || user !== null) return
    apiGet<AuthProviders>('/api/v1/auth/providers')
      .then((res) => setSso(res.oidc.enabled))
      .catch((err) => {
        console.warn('auth providers unavailable; hiding SSO login', err)
        setSso(false)
      })
  }, [booted, user])

  // The banner is open (the sign-in screen renders it too): fetch at boot,
  // refetch on login/logout, and poll so another admin's edit converges
  // everywhere within 30s. A failure keeps the last known value — the
  // banner must never block login or blank the app.
  useEffect(() => {
    let cancelled = false
    const load = () =>
      apiGet<UIBanner>('/api/v1/ui-banner')
        .then((b) => {
          if (!cancelled) setBanner(b)
        })
        .catch((err) => console.warn('ui banner unavailable; keeping last known', err))
    load()
    const id = setInterval(load, 30_000)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [user])

  // Any 401 from a view means the session died server-side: back to login.
  const onAuthError = useCallback((err: unknown) => {
    if (err instanceof ApiError && err.status === 401) setUser(null)
  }, [])

  const logout = useCallback(async () => {
    try {
      await apiPost('/api/v1/auth/logout')
    } catch {
      /* session may already be gone; either way we are logged out */
    }
    setCsrfToken('')
    setUser(null)
  }, [])

  if (!booted) {
    return (
      <BannerFrame banner={banner}>
        <div className="boot-state" role="status">
          <LogoMark className="logo-mark logo-mark-boot" />
          Loading PolarBEAM…
        </div>
      </BannerFrame>
    )
  }
  if (!user) {
    return (
      <BannerFrame banner={banner}>
        <Login
          sso={sso}
          onLogin={(res) => {
            setCsrfToken(res.csrf_token)
            setUser(res.user)
            setServerVersion(res.version)
          }}
        />
      </BannerFrame>
    )
  }

  return (
    <BannerFrame banner={banner}>
      <div className="app">
        <button className="skip-link" onClick={() => document.getElementById('main-content')?.focus()}>
          Skip to content
        </button>
        <header className="topbar">
          <a className="brand" href="#/">
            <LogoMark className="logo-mark logo-mark-header" />
            PolarBEAM
          </a>
          <nav className="topnav" aria-label="Primary navigation">
            {NAV.map((item) => (
              <a
                key={item.href}
                href={item.href}
                className={item.isActive(route) ? 'active' : ''}
                aria-current={item.isActive(route) ? 'page' : undefined}
              >
                {item.label}
              </a>
            ))}
          </nav>
          <div className="topbar-right">
            <TimezoneToggle />
            <ThemeToggle />
            <details className={'user-menu' + (route.view === 'settings' ? ' user-menu-current' : '')}>
              <summary aria-label={`Open user menu for ${user.username}`}>
                <svg className="user-menu-icon" viewBox="0 0 24 24" aria-hidden="true">
                  <circle cx="12" cy="8" r="3.5" />
                  <path d="M5.5 20c.5-4 2.7-6 6.5-6s6 2 6.5 6" />
                </svg>
              </summary>
              <div className="user-menu-popover">
                <div className="user-menu-identity">
                  <strong>{user.username}</strong>
                  <span>{user.role}</span>
                </div>
                {user.role === 'admin' && (
                  <a
                    href="#/settings"
                    aria-current={route.view === 'settings' ? 'page' : undefined}
                    onClick={(event) => event.currentTarget.closest('details')?.removeAttribute('open')}
                  >
                    Settings
                  </a>
                )}
                {/* Not admin-gated, unlike Settings: provenance and the server
                  build are useful to every role. */}
                <a
                  href="#/about"
                  aria-current={route.view === 'about' ? 'page' : undefined}
                  onClick={(event) => event.currentTarget.closest('details')?.removeAttribute('open')}
                >
                  About
                </a>
                {/* Federated accounts have no password here — their
                  credential lives at the IdP — so the item is absent, not
                  disabled. */}
                {user.auth_source !== 'oidc' && (
                  <button
                    type="button"
                    onClick={(event) => {
                      event.currentTarget.closest('details')?.removeAttribute('open')
                      setChangingPassword(true)
                    }}
                  >
                    Change password
                  </button>
                )}
                <button type="button" onClick={logout}>
                  Log out
                </button>
              </div>
            </details>
          </div>
        </header>
        {changingPassword && (
          <ChangePasswordDialog onClose={() => setChangingPassword(false)} onAuthError={onAuthError} />
        )}
        <main id="main-content" tabIndex={-1}>
          {route.view === 'overview' ? (
            <Overview onAuthError={onAuthError} />
          ) : route.view === 'pair' ? (
            // Keyed on the pair so switching pairs remounts with fresh state:
            // a stale series from the previous pair would otherwise keep
            // rendering under the new names when the new fetch fails.
            <PairDetail key={`${route.a}/${route.b}`} a={route.a} b={route.b} onAuthError={onAuthError} />
          ) : route.view === 'target' ? (
            // Keyed on the id for the same remount-on-switch reason.
            <TargetDetail key={route.id} id={route.id} onAuthError={onAuthError} />
          ) : route.view === 'targets' ? (
            <Targets onAuthError={onAuthError} />
          ) : route.view === 'incidents' ? (
            <Outages onAuthError={onAuthError} />
          ) : route.view === 'agents' ? (
            <Agents agent={route.agent} onAuthError={onAuthError} />
          ) : route.view === 'settings' ? (
            <Settings
              tab={route.tab}
              isAdmin={user.role === 'admin'}
              username={user.username}
              onAuthError={onAuthError}
              onBannerSaved={setBanner}
            />
          ) : route.view === 'about' ? (
            <About version={serverVersion} />
          ) : (
            <Paths onAuthError={onAuthError} />
          )}
        </main>
      </div>
    </BannerFrame>
  )
}
