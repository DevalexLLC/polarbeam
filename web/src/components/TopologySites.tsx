import { useEffect, useMemo, useState } from 'react'
import { fmtLatency } from '../format'
import { inheritRouteNetwork } from '../routeState'
import { SEVERITY_LABEL } from '../severity'
import { rankSiteTopology, type SiteTopology } from '../siteTopology'

export default function TopologySites({ topology }: { topology: SiteTopology[] }) {
  const ranked = useMemo(() => rankSiteTopology(topology), [topology])
  const [selected, setSelected] = useState<string | null>(null)

  useEffect(() => {
    if (selected && !topology.some((entry) => entry.site.name === selected)) setSelected(null)
  }, [selected, topology])

  if (ranked.length === 0) return <p className="muted">No sites are enrolled.</p>

  return (
    <ol className="topology-sites" aria-label="Sites ranked by health">
      {ranked.map(({ site, severity, stats }) => {
        const open = selected === site.name
        const label = site.display_name || site.name
        const detailID = `topology-site-${encodeURIComponent(site.name)}`
        return (
          <li key={site.name} className={`topology-site topology-site-${severity}`}>
            <button
              type="button"
              className="topology-site-summary"
              aria-label={`Select ${label}, ${SEVERITY_LABEL[severity]}`}
              aria-expanded={open}
              aria-controls={detailID}
              onClick={() => setSelected(open ? null : site.name)}
            >
              <span className={`map-legend-dot sev-${severity}`} aria-hidden="true" />
              <span className="topology-site-identity">
                <strong>{label}</strong>
                <span>{site.location || (site.display_name ? site.name : 'Location not set')}</span>
              </span>
              <span className="topology-site-health">
                <strong>{SEVERITY_LABEL[severity]}</strong>
                <span>
                  {stats.directions === 0
                    ? 'No monitored directions'
                    : `${stats.dirCounts.ok} of ${stats.directions} directions healthy`}
                </span>
              </span>
            </button>
            {open && (
              <div id={detailID} className="topology-site-detail">
                <span>
                  <strong>{stats.degree}</strong> {stats.degree === 1 ? 'link' : 'links'}
                </span>
                <span>
                  <strong>{stats.bestLatencyUs == null ? '—' : fmtLatency(stats.bestLatencyUs)}</strong> best live
                  latency
                </span>
                {stats.peers.length > 0 && (
                  <span className="topology-site-peers">
                    {stats.peers.map((peer) => (
                      <a
                        key={peer}
                        href={inheritRouteNetwork(
                          `#/pair/${encodeURIComponent(site.name)}/${encodeURIComponent(peer)}`,
                        )}
                      >
                        {site.name} ⇄ {peer}
                      </a>
                    ))}
                  </span>
                )}
              </div>
            )}
          </li>
        )
      })}
    </ol>
  )
}
