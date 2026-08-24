import { apiGet } from '../api'
import { fmtAgo } from '../format'
import { inheritRouteNetwork } from '../routeState'
import type { AgentBucketFailuresResponse, AgentHealthResponse, AgentInfo } from '../types'
import HealthStrip, { stripStats, UptimeValue } from './HealthStrip'

// Fleet health at a glance: one row per agent with its last update, 24 h
// probe-success strip, and uptime %. Agents absent from the health response
// have no results in the window — they show "—", never an invented 100 %.
// On multi-network installs each row carries its plane as a chip: site and
// hostname alone cannot tell two same-site agents on different planes apart.
export default function FleetAgentsCard({
  agents,
  health,
  multiNetwork,
}: {
  agents: AgentInfo[]
  health: AgentHealthResponse | null
  multiNetwork: boolean
}) {
  const bucketS = health?.bucket_s ?? 1800
  const nowS = Date.now() / 1000
  const healthById = new Map(health?.agents.map((a) => [a.id, a.buckets]) ?? [])

  return (
    <section className="card overview-fleet">
      <div className="card-head">
        <div>
          <span className="eyebrow">Fleet</span>
          <h2>Agents</h2>
        </div>
        <a className="text-link" href={inheritRouteNetwork('#/agents')}>
          View agents
        </a>
      </div>
      {agents.length === 0 ? (
        <div className="empty-state">
          <strong>No agents enrolled</strong>
          <span>Enroll an agent to start measuring.</span>
        </div>
      ) : (
        <div className="fleet-scroll">
          <table className="fleet-table">
            <thead>
              <tr>
                <th scope="col">Agent</th>
                <th scope="col">Last update</th>
                <th scope="col">24 h health</th>
                <th scope="col" className="fleet-uptime">
                  Uptime
                </th>
              </tr>
            </thead>
            <tbody>
              {agents.map((a) => {
                const buckets = healthById.get(a.id) ?? []
                const s = stripStats(buckets, bucketS, nowS)
                return (
                  <tr
                    key={a.id}
                    onClick={(e) => {
                      // The whole row is a convenience click target for the
                      // agent link, but never steal clicks meant for real
                      // controls — including strip slots, whose click pins
                      // the drill-down card rather than navigating.
                      if ((e.target as Element).closest('button, a, .fleet-strip, .strip-tip')) return
                      location.hash = inheritRouteNetwork('#/agents?agent=' + encodeURIComponent(a.id))
                    }}
                  >
                    <td>
                      {/* aria-label because the linter can't see the nested
                          interpolated text as the control's label. */}
                      <a
                        className="fleet-agent-link"
                        href={inheritRouteNetwork('#/agents?agent=' + encodeURIComponent(a.id))}
                        aria-label={
                          multiNetwork ? `${a.site} · ${a.hostname} · ${a.network}` : `${a.site} · ${a.hostname}`
                        }
                      >
                        <strong>
                          {a.site}
                          {multiNetwork && <span className="chip">{a.network}</span>}
                        </strong>
                        <small>{a.hostname}</small>
                      </a>
                    </td>
                    <td className="fleet-seen">{a.last_seen_at ? fmtAgo(a.last_seen_at) : 'never'}</td>
                    <td className="fleet-health">
                      {/* The strip stays dumb about 401s: a dead session
                          shows in the pinned card once, and the 30 s polls
                          bounce the app to login within a cycle anyway. */}
                      <HealthStrip
                        buckets={s.inWindow}
                        bucketS={bucketS}
                        endS={s.endS}
                        label={s.stripLabel}
                        fetchSlotDetail={(t) =>
                          apiGet<AgentBucketFailuresResponse>(`/api/v1/agents/${a.id}/health/bucket?t=${t}`)
                        }
                      />
                    </td>
                    <td className="fleet-uptime">
                      <UptimeValue uptime={s.uptime} partial={s.partial} stripLabel={s.stripLabel} />
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
