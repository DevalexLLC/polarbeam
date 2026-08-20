import { useCallback, useEffect, useMemo, useState } from 'react'
import { apiGet } from '../api'
import type { Caps } from '../caps'
import { planeChoice } from '../plane'
import { SETTINGS_TABS, visibleTabs } from '../settingsTabs'
import type { SettingsTab } from '../settingsTabs'
import BannerSettingsPanel from '../components/BannerSettingsPanel'
import EnrollmentPanel from '../components/EnrollmentPanel'
import MeshesPanel from '../components/MeshesPanel'
import NetworksPanel from '../components/NetworksPanel'
import OIDCSettingsPanel from '../components/OIDCSettingsPanel'
import PathThresholdsPanel from '../components/PathThresholdsPanel'
import ProbesPanel from '../components/ProbesPanel'
import SitesPanel from '../components/SitesPanel'
import TargetsPanel from '../components/TargetsPanel'
import ThresholdSettingsPanel from '../components/ThresholdSettings'
import UsersPanel from '../components/UsersPanel'
import type { SettingsResponse, UIBanner } from '../types'

const POLL_MS = 30_000

export default function Settings({
  tab,
  caps,
  networks,
  username,
  onAuthError,
  onBannerSaved,
}: {
  tab: SettingsTab
  caps: Caps
  networks: string[]
  username: string
  onAuthError: (err: unknown) => void
  onBannerSaved: (b: UIBanner) => void
}) {
  const [settings, setSettings] = useState<SettingsResponse | null>(null)
  const [error, setError] = useState('')

  const tabs = useMemo(() => visibleTabs(caps), [caps])

  // Resolved once, so "which plane does this write name" has exactly one
  // definition. The workload surfaces always belong to a plane; targets and
  // the all-planes threshold override additionally have an operator-owned
  // "no plane" row, which is what allowGlobal offers — to a global caller
  // only, since those rows are adminWrite.
  const workloadPlane = useMemo(() => planeChoice(caps, networks), [caps, networks])
  const ownedPlane = useMemo(() => planeChoice(caps, networks, { allowGlobal: true }), [caps, networks])

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
          <p>{SETTINGS_TABS.find((t) => t.tab === tab)?.intro}</p>
        </div>
      </div>
      <nav className="settings-tabs" aria-label="Settings sections">
        {tabs.map((t) => (
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
      {/* Every panel's gate is named here, on one screen, so the client-side
        dispositions can be read against httpapi.go's route table at a
        glance. adminWrite and networkWrite are the server's own wrapper
        names; a panel never sees `caps` and so cannot pick the wrong one. */}
      {tab === 'authentication' ? (
        <OIDCSettingsPanel caps={caps} canWrite={caps.adminWrite} onAuthError={onAuthError} />
      ) : tab === 'banner' ? (
        <BannerSettingsPanel caps={caps} canWrite={caps.adminWrite} onAuthError={onAuthError} onSaved={onBannerSaved} />
      ) : tab === 'users' ? (
        <UsersPanel caps={caps} canWrite={caps.adminWrite} currentUsername={username} onAuthError={onAuthError} />
      ) : tab === 'networks' ? (
        <NetworksPanel canWrite={caps.adminWrite} onAuthError={onAuthError} />
      ) : tab === 'sites' ? (
        <SitesPanel canWrite={caps.adminWrite} onAuthError={onAuthError} />
      ) : tab === 'enrollment' ? (
        <EnrollmentPanel caps={caps} canWrite={caps.networkWrite} plane={workloadPlane} onAuthError={onAuthError} />
      ) : tab === 'targets' ? (
        <TargetsPanel caps={caps} canWrite={caps.networkWrite} plane={ownedPlane} onAuthError={onAuthError} />
      ) : tab === 'meshes' ? (
        <MeshesPanel canWrite={caps.networkWrite} plane={workloadPlane} onAuthError={onAuthError} />
      ) : tab === 'probes' ? (
        <ProbesPanel canWrite={caps.networkWrite} plane={workloadPlane} onAuthError={onAuthError} />
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
              Values at or above the degraded threshold require attention. Critical thresholds are a stronger visual
              signal inside detailed connectivity views, and sustained critical breaches (3 consecutive results) open
              degraded incidents on the Incidents page.
            </p>
            <ThresholdSettingsPanel
              settings={settings}
              canWrite={caps.adminWrite}
              onSaved={setSettings}
              onAuthError={onAuthError}
              variant="page"
            />
          </section>
          <PathThresholdsPanel
            settings={settings}
            caps={caps}
            canWrite={caps.networkWrite}
            plane={ownedPlane}
            onChanged={load}
            onAuthError={onAuthError}
          />
        </>
      )}
    </>
  )
}
