import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ApiError, apiGet, apiPost, setCsrfToken } from './api'
import { capsOf, roleLabel } from './caps'
import { PRIMARY_NAVIGATION, SETTINGS_NAVIGATION, navigationItemIsCurrent } from './navigation'
import { reconcileNetworkFilter } from './networkFilter'
import { documentTitle } from './pageState'
import {
  inheritRouteNetwork,
  replaceCanonicalRoute,
  routeParam,
  subscribeRouteState,
  updateRouteParams,
} from './routeState'
import { SETTINGS_TABS, canOpenSettings, resolveTab, settingsTabDef } from './settingsTabs'
import { useSettingsMutation } from './settingsMutation'
import type { SettingsTab } from './settingsTabs'
import type { AuthProviders, LoginResponse, NetworksConfigResponse, UIBanner, User } from './types'
import BannerFrame from './components/BannerFrame'
import ChangePasswordDialog from './components/ChangePasswordDialog'
import LogoMark from './components/LogoMark'
import MobileNavigation from './components/MobileNavigation'
import PageError from './components/PageError'
import ThemeToggle from './components/ThemeToggle'
import TimezoneToggle from './components/TimezoneToggle'
import TopbarFilter from './components/TopbarFilter'
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
// The tab vocabulary itself lives in settingsTabs.ts, which also owns which
// role may open each one.

type Route =
  | { view: 'overview' }
  | { view: 'pair'; a: string; b: string }
  | { view: 'target'; id: string }
  | { view: 'targets' }
  | { view: 'incidents' }
  | { view: 'routes' }
  | { view: 'agents'; agent: string | null }
  | { view: 'about' }
  | { view: 'settings'; tab: string }
  | { view: 'not-found'; path: string }

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
  const parts = hash.replace(/^#\/?/, '').split('?')[0].split('/')
  if (parts[0] === '' || parts[0] === 'connectivity' || parts[0] === 'sightlines') return { view: 'overview' }
  if (/^sso-error=[a-z-]+$/.test(parts[0])) return { view: 'overview' }
  if (parts[0] === 'pair' && parts[1] && parts[2]) {
    return { view: 'pair', a: decodeSegment(parts[1]), b: decodeSegment(parts[2]) }
  }
  if (parts[0] === 'targets') return { view: 'targets' }
  // #/target/<id> is the per-target drill-down; a bad id shows the view's
  // own loud not-found rather than silently landing on the overview.
  if (parts[0] === 'target') {
    return { view: 'target', id: parts[1] ? decodeSegment(parts[1]) : '' }
  }
  if (parts[0] === 'incidents' || parts[0] === 'outages') return { view: 'incidents' }
  if (parts[0] === 'routes' || parts[0] === 'paths') return { view: 'routes' }
  if (parts[0] === 'agents') return { view: 'agents', agent: routeParam(hash, 'agent') || null }
  if (parts[0] === 'about') return { view: 'about' }
  if (parts[0] === 'settings') {
    // The section is kept raw. Which tabs exist AND which this caller may
    // open is a question only answerable once the session is known, so it is
    // resolved at render (resolveTab) rather than here: parseHash runs from
    // a useState initializer before /auth/me returns.
    return {
      view: 'settings',
      tab: routeParam(hash, 'subsection') || routeParam(hash, 'section') || decodeSegment(parts[1] ?? ''),
    }
  }
  return { view: 'not-found', path: parts.map(decodeSegment).join('/') }
}

// Focus follows page navigation, not in-page state. Agent disclosure is
// encoded in the hash for shareability, but expanding a row must leave focus
// on that row; detail identities and Settings subsections are distinct pages.
function routePageKey(route: Route): string {
  if (route.view === 'pair') return `${route.view}/${route.a}/${route.b}`
  if (route.view === 'target') return `${route.view}/${route.id}`
  if (route.view === 'settings') return `${route.view}/${route.tab}`
  return route.view
}

function routeTitle(route: Route, settingsTab: SettingsTab | null): string {
  if (route.view === 'overview') return 'Overview'
  if (route.view === 'pair') return `${route.a} ⇄ ${route.b}`
  if (route.view === 'target') return 'Target detail'
  if (route.view === 'targets') return 'Targets'
  if (route.view === 'incidents') return 'Incidents'
  if (route.view === 'routes') return 'Routes'
  if (route.view === 'agents') return route.agent ? 'Agent detail' : 'Agents'
  if (route.view === 'about') return 'About'
  if (route.view === 'not-found') return 'Page not found'
  const section = SETTINGS_TABS.find((item) => item.tab === settingsTab)
  return section ? `${section.label} · Settings` : 'Settings'
}

export default function App() {
  const { clearNotifications, guardAction } = useSettingsMutation()
  const [booted, setBooted] = useState(false)
  const [user, setUser] = useState<User | null>(null)
  const [serverVersion, setServerVersion] = useState('')
  const [sso, setSso] = useState(false)
  const [banner, setBanner] = useState<UIBanner | null>(null)
  const [changingPassword, setChangingPassword] = useState(false)
  // null until this session's /config/networks has succeeded — deliberately
  // distinct from [], which means "this caller sees no networks". Plane
  // pickers must not read "not loaded yet" as "single-network install".
  const [networks, setNetworks] = useState<string[] | null>(null)
  const [route, setRoute] = useState<Route>(() => parseHash(location.hash))
  const routeKey = JSON.stringify(route)
  const pageKey = routePageKey(route)
  const initialRouteRef = useRef(true)
  const [entityTitle, setEntityTitle] = useState<{ routeKey: string; label: string } | null>(null)

  useEffect(() => {
    const onRoute = () => {
      setRoute(parseHash(replaceCanonicalRoute({}, false)))
    }
    onRoute()
    return subscribeRouteState(onRoute)
  }, [])

  // Navigation moves keyboard focus to the new page's heading. Views that
  // wait for their first response are observed until their heading mounts;
  // the observer is discarded on the next route change.
  useEffect(() => {
    if (initialRouteRef.current) {
      initialRouteRef.current = false
      return
    }
    const main = document.getElementById('main-content')
    if (!main) return
    let observer: MutationObserver | null = null
    let timeout: number | null = null
    const stopWaiting = () => {
      observer?.disconnect()
      observer = null
      if (timeout !== null) window.clearTimeout(timeout)
      timeout = null
      document.removeEventListener('focusin', stopWaiting, true)
      document.removeEventListener('pointerdown', stopWaiting, true)
      document.removeEventListener('keydown', stopWaiting, true)
    }
    const focusHeading = () => {
      const heading = main.querySelector<HTMLElement>('h1, h2')
      if (!heading) return false
      stopWaiting()
      const previousTabIndex = heading.getAttribute('tabindex')
      heading.tabIndex = -1
      heading.focus()
      heading.addEventListener(
        'blur',
        () => {
          if (previousTabIndex === null) heading.removeAttribute('tabindex')
          else heading.setAttribute('tabindex', previousTabIndex)
        },
        { once: true },
      )
      return true
    }
    const frame = requestAnimationFrame(() => {
      if (focusHeading()) return
      observer = new MutationObserver(focusHeading)
      observer.observe(main, { childList: true, subtree: true })
      document.addEventListener('focusin', stopWaiting, true)
      document.addEventListener('pointerdown', stopWaiting, true)
      document.addEventListener('keydown', stopWaiting, true)
      timeout = window.setTimeout(stopWaiting, 5_000)
    })
    return () => {
      cancelAnimationFrame(frame)
      stopWaiting()
    }
  }, [pageKey])

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

  // The network list decides whether the top-bar filter renders at all
  // (single-network installs never see it). Fetch on login and poll so an
  // admin adding or deleting a plane converges everywhere within 30s; each
  // fetch also reconciles a URL filter that may have gone stale. A
  // failure keeps the last known list — filtering must never block the app.
  useEffect(() => {
    // Drop the previous session's list before fetching this one's. The
    // names are another tenant's topology to a scoped user, and "keep the
    // last known on failure" — right within a session — would otherwise
    // show them across a logout, or indefinitely if the new fetch fails.
    setNetworks(null)
    if (!user) return
    let cancelled = false
    const load = () =>
      apiGet<NetworksConfigResponse>('/api/v1/config/networks')
        .then((res) => {
          if (cancelled) return
          const names = res.networks.map((n) => n.name)
          setNetworks(names)
          reconcileNetworkFilter(names)
          // A scoped session that started with no networks is looking at
          // the "no networks assigned" wall. The server applies a scope
          // change live, so the moment this poll returns a plane, the
          // session identity is stale — re-read it rather than making the
          // user reload to escape a wall an admin has already cleared.
          if (user.networks !== null && user.networks.length === 0 && names.length > 0) {
            apiGet<LoginResponse>('/api/v1/auth/me')
              .then((me) => {
                if (!cancelled) setUser(me.user)
              })
              .catch(() => {
                /* keep the wall; the next poll tries again */
              })
          }
        })
        .catch((err) => console.warn('networks unavailable; keeping last known', err))
    load()
    const id = setInterval(load, 30_000)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [user])

  // Capabilities are derived from the session, not stored: they change
  // exactly once per session (at login/logout), and App re-renders its whole
  // tree at that moment anyway. A store would add a global that can drift
  // from `user`, and would make logout ordering a correctness bug.
  const caps = useMemo(() => (user ? capsOf(user) : null), [user])
  useEffect(() => clearNotifications(), [clearNotifications, user])
  const primaryNavigation = PRIMARY_NAVIGATION.map((item) => ({
    ...item,
    href: inheritRouteNetwork(item.href),
    current: navigationItemIsCurrent(item.href, route.view),
    sameRoute: JSON.stringify(parseHash(item.href)) === routeKey,
  }))
  const mobileNavigation =
    caps !== null && canOpenSettings(caps)
      ? [
          ...primaryNavigation,
          {
            ...SETTINGS_NAVIGATION,
            href: inheritRouteNetwork(SETTINGS_NAVIGATION.href),
            current: navigationItemIsCurrent(SETTINGS_NAVIGATION.href, route.view),
            sameRoute: JSON.stringify(parseHash(SETTINGS_NAVIGATION.href)) === routeKey,
          },
        ]
      : primaryNavigation

  // A hash naming a tab this role cannot open is rewritten to the one it
  // landed on, so the URL the user copies is honest. replaceState does not
  // fire hashchange, so this cannot loop.
  const settingsTab = caps && route.view === 'settings' ? resolveTab(route.tab, caps) : null
  useEffect(() => {
    if (route.view !== 'settings' || settingsTab === null) return
    if (route.tab === settingsTab) return
    const destination = settingsTabDef(settingsTab)
    updateRouteParams({ section: destination.group, subsection: destination.tab }, 'replace')
    setRoute({ view: 'settings', tab: settingsTab })
  }, [route, settingsTab])

  const reportEntityTitle = useCallback((label: string) => setEntityTitle({ routeKey, label }), [routeKey])
  const pageTitle = entityTitle?.routeKey === routeKey ? entityTitle.label : routeTitle(route, settingsTab)
  useEffect(() => {
    document.title = documentTitle(!booted ? '' : user ? pageTitle : 'Sign in')
  }, [booted, pageTitle, user])

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
          <a className="brand" href={inheritRouteNetwork('#/')}>
            <LogoMark className="logo-mark logo-mark-header" />
            <span className="brand-name">PolarBEAM</span>
          </a>
          <MobileNavigation items={mobileNavigation} routeKey={routeKey} />
          <nav className="topnav" aria-label="Primary navigation">
            {primaryNavigation.map((item) => (
              <a
                key={item.href}
                href={item.href}
                className={item.current ? 'active' : ''}
                aria-current={item.current ? 'page' : undefined}
              >
                {item.label}
              </a>
            ))}
          </nav>
          <div className="topbar-right">
            <TopbarFilter networks={networks ?? []} scope={caps?.networks ?? null} />
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
                  <span>{roleLabel(user.role)}</span>
                  {/* A scoped account's planes are part of its identity:
                    what it can see and where its writes land. */}
                  {user.networks !== null && (
                    <span className="user-menu-scope">
                      {user.networks.length === 0 ? 'no networks assigned' : user.networks.join(', ')}
                    </span>
                  )}
                </div>
                {caps !== null && canOpenSettings(caps) && (
                  <a
                    href={inheritRouteNetwork('#/settings')}
                    aria-current={route.view === 'settings' ? 'page' : undefined}
                    onClick={(event) => event.currentTarget.closest('details')?.removeAttribute('open')}
                  >
                    Settings
                  </a>
                )}
                {/* Not admin-gated, unlike Settings: provenance and the server
                  build are useful to every role. */}
                <a
                  href={inheritRouteNetwork('#/about')}
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
                <button type="button" onClick={() => guardAction(() => void logout())}>
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
          {caps !== null && caps.networks !== null && caps.networks.length === 0 ? (
            // A scoped account with an empty set is a real server state — a
            // mapping that matched nothing, or a network deleted out from
            // under it. Every view would render blank and every write would
            // 404, so say why once instead of ten times.
            <div className="state-panel">
              <h2>No networks assigned</h2>
              <p>
                Your account is limited to specific networks, but none are assigned to it yet. Ask an administrator to
                assign one.
              </p>
            </div>
          ) : route.view === 'overview' ? (
            <Overview onAuthError={onAuthError} />
          ) : route.view === 'pair' ? (
            // Keyed on the pair so switching pairs remounts with fresh state:
            // a stale series from the previous pair would otherwise keep
            // rendering under the new names when the new fetch fails.
            <PairDetail key={`${route.a}/${route.b}`} a={route.a} b={route.b} onAuthError={onAuthError} />
          ) : route.view === 'target' ? (
            // Keyed on the id for the same remount-on-switch reason.
            <TargetDetail key={route.id} id={route.id} onAuthError={onAuthError} onTitleChange={reportEntityTitle} />
          ) : route.view === 'targets' ? (
            <Targets onAuthError={onAuthError} />
          ) : route.view === 'incidents' ? (
            <Outages onAuthError={onAuthError} />
          ) : route.view === 'agents' ? (
            <Agents
              agent={route.agent}
              networks={networks ?? []}
              onAuthError={onAuthError}
              onTitleChange={reportEntityTitle}
            />
          ) : route.view === 'settings' ? (
            // The link is hidden for roles that may open no tab, but the
            // hash is still typeable; say so rather than rendering an empty
            // shell.
            caps === null || settingsTab === null ? (
              <div className="state-panel">
                <h2>Nothing to configure</h2>
                <p>Your role has no settings to manage. Configuration is shown on the pages it applies to.</p>
              </div>
            ) : (
              <Settings
                tab={settingsTab}
                caps={caps}
                networks={networks}
                username={user.username}
                onAuthError={onAuthError}
                onBannerSaved={setBanner}
              />
            )
          ) : route.view === 'about' ? (
            <About version={serverVersion} />
          ) : route.view === 'routes' ? (
            <Paths onAuthError={onAuthError} />
          ) : (
            <PageError
              title="Page not found"
              subject="page"
              message="This address does not match a PolarBEAM page."
              backHref={inheritRouteNetwork('#/')}
              backLabel="Back to Overview"
            />
          )}
        </main>
      </div>
    </BannerFrame>
  )
}
