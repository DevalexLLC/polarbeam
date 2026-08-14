import { useCallback, useEffect, useState } from 'react'
import { apiGet } from '../api'
import type { SettingsTab } from '../App'
import BannerSettingsPanel from '../components/BannerSettingsPanel'
import EnrollmentPanel from '../components/EnrollmentPanel'
import MeshesPanel from '../components/MeshesPanel'
import OIDCSettingsPanel from '../components/OIDCSettingsPanel'
import PathThresholdsPanel from '../components/PathThresholdsPanel'
import ProbesPanel from '../components/ProbesPanel'
import SitesPanel from '../components/SitesPanel'
import TargetsPanel from '../components/TargetsPanel'
import ThresholdSettingsPanel from '../components/ThresholdSettings'
import UsersPanel from '../components/UsersPanel'
import type { SettingsResponse, UIBanner } from '../types'

const POLL_MS = 30_000

const TABS: Array<{ tab: SettingsTab; href: string; label: string }> = [
  { tab: 'thresholds', href: '#/settings', label: 'Thresholds' },
  { tab: 'sites', href: '#/settings/sites', label: 'Sites' },
  { tab: 'targets', href: '#/settings/targets', label: 'Targets' },
  { tab: 'meshes', href: '#/settings/meshes', label: 'Meshes' },
  { tab: 'probes', href: '#/settings/probes', label: 'Probes' },
  { tab: 'enrollment', href: '#/settings/enrollment', label: 'Enrollment' },
  { tab: 'users', href: '#/settings/users', label: 'Users' },
  { tab: 'authentication', href: '#/settings/authentication', label: 'Authentication' },
  { tab: 'banner', href: '#/settings/banner', label: 'Banner' },
]

const TAB_INTRO: Record<SettingsTab, string> = {
  thresholds: 'Shared thresholds used to classify network health across the dashboard.',
  sites: 'The locations agents enroll into, with optional map placement and display metadata.',
  targets: 'External hosts and URLs that site agents probe.',
  meshes: 'Site groups whose members probe each other in both directions.',
  probes: 'The measurement workload pushed to every affected agent within ~30 seconds.',
  enrollment: 'Single-use join tokens that enroll new agents into a site.',
  users: 'Dashboard accounts across local and single sign-on, with sign-in activity.',
  authentication: 'Optional single sign-on via an OpenID Connect provider. Local accounts always keep working.',
  banner: 'An optional marking shown at the top and bottom of every screen, the sign-in page included.',
}

export default function Settings({
  tab,
  isAdmin,
  username,
  onAuthError,
  onBannerSaved,
}: {
  tab: SettingsTab
  isAdmin: boolean
  username: string
  onAuthError: (err: unknown) => void
  onBannerSaved: (b: UIBanner) => void
}) {
  const [settings, setSettings] = useState<SettingsResponse | null>(null)
  const [error, setError] = useState('')

  // Poll like every other view: a transient failure retries on the next
  // tick, and another admin's change converges here ≤30 s. The panel keeps
  // its own draft once edited, so a poll never clobbers in-progress input.
  // Only the thresholds tab needs /settings; the config tabs poll their own
  // endpoints.
  // Hoisted so the overrides panel can force an immediate refetch after a
  // write instead of waiting out the poll interval.
  const load = useCallback(() => {
    apiGet<SettingsResponse>('/api/v1/settings')
      .then((s) => {
        setSettings(s)
        setError('')
      })
      .catch((err) => {
        onAuthError(err)
        setError(err instanceof Error ? err.message : String(err))
      })
  }, [onAuthError])

  useEffect(() => {
    if (tab !== 'thresholds') return
    load()
    const id = setInterval(load, POLL_MS)
    return () => clearInterval(id)
  }, [tab, load])

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">Administration</div>
          <h1>Settings</h1>
          <p>{TAB_INTRO[tab]}</p>
        </div>
      </div>
      <nav className="settings-tabs" aria-label="Settings sections">
        {TABS.map((t) => (
          <a
            key={t.tab}
            href={t.href}
            className={t.tab === tab ? 'active' : ''}
            aria-current={t.tab === tab ? 'page' : undefined}
          >
            {t.label}
          </a>
        ))}
      </nav>
      {tab === 'authentication' ? (
        <OIDCSettingsPanel isAdmin={isAdmin} onAuthError={onAuthError} />
      ) : tab === 'banner' ? (
        <BannerSettingsPanel isAdmin={isAdmin} onAuthError={onAuthError} onSaved={onBannerSaved} />
      ) : tab === 'users' ? (
        <UsersPanel isAdmin={isAdmin} currentUsername={username} onAuthError={onAuthError} />
      ) : tab === 'sites' ? (
        <SitesPanel isAdmin={isAdmin} onAuthError={onAuthError} />
      ) : tab === 'enrollment' ? (
        <EnrollmentPanel isAdmin={isAdmin} onAuthError={onAuthError} />
      ) : tab === 'targets' ? (
        <TargetsPanel isAdmin={isAdmin} onAuthError={onAuthError} />
      ) : tab === 'meshes' ? (
        <MeshesPanel isAdmin={isAdmin} onAuthError={onAuthError} />
      ) : tab === 'probes' ? (
        <ProbesPanel isAdmin={isAdmin} onAuthError={onAuthError} />
      ) : error && !settings ? (
        <div className="state-panel state-error">
          <h2>Settings unavailable</h2>
          <p>{error}</p>
        </div>
      ) : !settings ? (
        <div className="state-panel" role="status">
          <span className="state-spinner" />
          Loading settings…
        </div>
      ) : (
        <>
          {error && (
            <div className="inline-alert" role="status">
              Refresh failed. Showing the last successful snapshot.
            </div>
          )}
          <section className="card settings-card">
            <div className="card-head">
              <div>
                <span className="eyebrow">Health classification</span>
                <h2>Connectivity thresholds</h2>
              </div>
            </div>
            <p className="section-intro">
              Values at or above the degraded threshold require attention. Critical thresholds remain a stronger visual
              signal inside detailed connectivity views.
            </p>
            <ThresholdSettingsPanel
              settings={settings}
              isAdmin={isAdmin}
              onSaved={setSettings}
              onAuthError={onAuthError}
              variant="page"
            />
          </section>
          <PathThresholdsPanel settings={settings} isAdmin={isAdmin} onChanged={load} onAuthError={onAuthError} />
        </>
      )}
    </>
  )
}
