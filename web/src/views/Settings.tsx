import { useMemo } from 'react'
import type { Caps } from '../caps'
import { planeChoice } from '../plane'
import { inheritRouteNetwork, navigateRouteHash } from '../routeState'
import { useRouteParam } from '../useRouteState'
import { SETTINGS_GROUPS, visibleTabs } from '../settingsTabs'
import type { SettingsTab } from '../settingsTabs'
import BannerSettingsPanel from '../components/BannerSettingsPanel'
import EnrollmentPanel from '../components/EnrollmentPanel'
import MeshesPanel from '../components/MeshesPanel'
import NetworksPanel from '../components/NetworksPanel'
import OIDCSettingsPanel from '../components/OIDCSettingsPanel'
import NetworkThresholdsPanel from '../components/NetworkThresholdsPanel'
import PathThresholdsPanel from '../components/PathThresholdsPanel'
import ProbesPanel from '../components/ProbesPanel'
import SettingsPageError from '../components/SettingsPageError'
import SitesPanel from '../components/SitesPanel'
import TargetsPanel from '../components/TargetsPanel'
import ThresholdSettingsPanel from '../components/ThresholdSettings'
import UsersPanel from '../components/UsersPanel'
import type { SettingsResponse, UIBanner } from '../types'
import { usePolledResource } from '../usePolledResource'

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
  // null while this session's network list is still loading or failed.
  // Plane pickers must refuse to guess rather than treat it as one network.
  networks: string[] | null
  username: string
  onAuthError: (err: unknown) => void
  onBannerSaved: (b: UIBanner) => void
}) {
  const [selectedSite, setSelectedSite] = useRouteParam('site')
  const [selectedProbe, setSelectedProbe] = useRouteParam('probe')

  const tabs = useMemo(() => visibleTabs(caps), [caps])

  // Resolved once, so "which plane does this write name" has exactly one
  // definition. The workload surfaces always belong to a plane; targets and
  // the all-planes threshold override additionally have an operator-owned
  // "no plane" row, which is what allowGlobal offers — to a global caller
  // only, since those rows are adminWrite.
  const workloadPlane = useMemo(() => planeChoice(caps, networks), [caps, networks])
  const ownedPlane = useMemo(() => planeChoice(caps, networks, { allowGlobal: true }), [caps, networks])
  // Panels that only need the raw list (admin-only surfaces) can treat
  // not-yet-loaded as empty; the plane pickers above cannot.
  const knownNetworks = networks ?? []

  // Poll like every other view: a transient failure retries on the next
  // tick, and another admin's change converges here ≤30 s. The panel keeps
  // its own draft once edited, so a poll never clobbers in-progress input.
  // Only the thresholds tab needs /settings; the config tabs poll their own
  // endpoints. reload lets the overrides panel force an immediate refetch
  // after a write instead of waiting out the poll interval.
  const {
    data: settings,
    error,
    reload,
  } = usePolledResource<SettingsResponse>('/api/v1/settings', {
    enabled: tab === 'thresholds',
    onAuthError,
    logLabel: 'settings',
  })

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <h1>Settings</h1>
        </div>
      </div>
      <div className="settings-mobile-picker">
        <label htmlFor="settings-section">Settings section</label>
        <select
          id="settings-section"
          value={tab}
          onChange={(event) => {
            const destination = tabs.find((item) => item.tab === event.target.value)
            if (destination) navigateRouteHash(inheritRouteNetwork(destination.href))
          }}
        >
          {SETTINGS_GROUPS.map((group) => {
            const children = tabs.filter((item) => item.group === group.group)
            return children.length === 0 ? null : (
              <optgroup key={group.group} label={group.label}>
                {children.map((item) => (
                  <option key={item.tab} value={item.tab}>
                    {item.label}
                  </option>
                ))}
              </optgroup>
            )
          })}
        </select>
      </div>
      <div className="settings-layout">
        <nav className="settings-sidebar" aria-label="Settings sections">
          {SETTINGS_GROUPS.map((group) => {
            const children = tabs.filter((item) => item.group === group.group)
            return children.length === 0 ? null : (
              <div className="settings-nav-group" key={group.group}>
                <div className="label">{group.label}</div>
                {children.map((item) => (
                  <a
                    key={item.tab}
                    href={inheritRouteNetwork(item.href)}
                    className={item.tab === tab ? 'active' : ''}
                    aria-current={item.tab === tab ? 'page' : undefined}
                  >
                    {item.label}
                  </a>
                ))}
              </div>
            )
          })}
        </nav>
        <div className="settings-content">
          {/* Every panel's gate is named here, on one screen, so the client-side
        dispositions can be read against httpapi.go's route table at a
        glance. adminWrite and networkWrite are the server's own wrapper
        names; a panel never sees `caps` and so cannot pick the wrong one. */}
          {tab === 'authentication' ? (
            <OIDCSettingsPanel
              caps={caps}
              canWrite={caps.adminWrite}
              networks={knownNetworks}
              onAuthError={onAuthError}
            />
          ) : tab === 'banner' ? (
            <BannerSettingsPanel
              caps={caps}
              canWrite={caps.adminWrite}
              onAuthError={onAuthError}
              onSaved={onBannerSaved}
            />
          ) : tab === 'users' ? (
            <UsersPanel
              caps={caps}
              canWrite={caps.adminWrite}
              networks={knownNetworks}
              currentUsername={username}
              onAuthError={onAuthError}
            />
          ) : tab === 'networks' ? (
            <NetworksPanel canWrite={caps.adminWrite} onAuthError={onAuthError} />
          ) : tab === 'sites' ? (
            <SitesPanel
              canWrite={caps.adminWrite}
              selectedSite={selectedSite}
              onSelectedSite={setSelectedSite}
              onAuthError={onAuthError}
            />
          ) : tab === 'enrollment' ? (
            <EnrollmentPanel caps={caps} canWrite={caps.networkWrite} plane={workloadPlane} onAuthError={onAuthError} />
          ) : tab === 'targets' ? (
            <TargetsPanel caps={caps} canWrite={caps.networkWrite} plane={ownedPlane} onAuthError={onAuthError} />
          ) : tab === 'meshes' ? (
            <MeshesPanel canWrite={caps.networkWrite} plane={workloadPlane} onAuthError={onAuthError} />
          ) : tab === 'probes' ? (
            <ProbesPanel
              canWrite={caps.networkWrite}
              plane={workloadPlane}
              selectedProbe={selectedProbe}
              onSelectedProbe={setSelectedProbe}
              onAuthError={onAuthError}
            />
          ) : error && !settings ? (
            <SettingsPageError
              title="Settings unavailable"
              subject="settings"
              error={error}
              onRetry={() => void reload()}
            />
          ) : !settings ? (
            <div className="state-panel" role="status">
              <span className="state-spinner" />
              Loading settings…
            </div>
          ) : (
            <>
              {error !== null && (
                <div className="inline-alert" role="status">
                  Refresh failed. Showing the last successful snapshot.
                </div>
              )}
              <section className="card settings-card">
                <div className="card-head">
                  <div>
                    <h2>Connectivity thresholds</h2>
                  </div>
                </div>
                <p className="section-intro">
                  Values at or above the degraded threshold require attention. Critical thresholds are a stronger visual
                  signal inside detailed connectivity views, and sustained critical breaches (3 consecutive results)
                  open degraded incidents on the Incidents page.
                </p>
                <ThresholdSettingsPanel
                  settings={settings}
                  canWrite={caps.adminWrite}
                  onSaved={() => void reload()}
                  onAuthError={onAuthError}
                  variant="page"
                />
              </section>
              {/* Ordered as the resolver folds them: global, then per-network,
            then per-pair — most general first, so the page reads the way a
            severity is actually decided. */}
              <NetworkThresholdsPanel
                settings={settings}
                caps={caps}
                canWrite={caps.networkWrite}
                plane={ownedPlane}
                onChanged={() => void reload()}
                onAuthError={onAuthError}
              />
              <PathThresholdsPanel
                settings={settings}
                caps={caps}
                canWrite={caps.networkWrite}
                plane={ownedPlane}
                onChanged={() => void reload()}
                onAuthError={onAuthError}
              />
            </>
          )}
        </div>
      </div>
    </>
  )
}
