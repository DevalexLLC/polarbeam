import { useEffect, useMemo, useState } from 'react'
import { apiGet } from '../api'
import { fmtAgo, fmtTime } from '../format'
import { useTimezone } from '../timezone'
import type {
  AgentInfo,
  AgentsResponse,
  OutagesResponse,
  ProbesConfigResponse,
  TargetConfig,
  TargetsConfigResponse,
} from '../types'

const POLL_MS = 30_000

// The browseable, read-only counterpart to Settings → Targets: every
// authenticated user can reach the per-target drill-down (#/target/<id>)
// from here. All four feeds are any-session reads; the joins are by
// target NAME because probe configs and outages carry names, not ids.
interface Feeds {
  targets: TargetConfig[]
  probeSites: Map<string, string[]> // target name -> distinct probing sites
  agentByID: Map<string, AgentInfo>
  troubled: Set<string> // target names with an open incident
}

// Agent-kind targets are enrollment-managed rows named agent:<agent-uuid>;
// resolve the agent for a human label (site + hostname).
function agentIDOf(t: TargetConfig): string | null {
  return t.kind === 'agent' && t.name.startsWith('agent:') ? t.name.slice('agent:'.length) : null
}

// active: the target has at least one ENABLED direct probe (external
// targets; probe_count can't tell — it counts disabled configs too).
// Agent targets pass true: mesh templates probe them without config rows.
function StatusCell({ t, troubled, active }: { t: TargetConfig; troubled: Set<string>; active: boolean }) {
  if (troubled.has(t.name)) {
    return (
      <span>
        <span className="dot swatch status-down" /> incident
      </span>
    )
  }
  if (!active) return <span className="hint">unprobed</span>
  // "no incidents" rather than "ok": this cell is fed by the incident
  // feed alone, which says nothing about targets that were never measured.
  return (
    <span>
      <span className="dot swatch status-ok" /> no incidents
    </span>
  )
}

export default function Targets({ onAuthError }: { onAuthError: (err: unknown) => void }) {
  useTimezone() // re-render fmtTime tooltips on UTC/local toggle
  const [feeds, setFeeds] = useState<Feeds | null>(null)
  const [error, setError] = useState('')
  const [query, setQuery] = useState('')

  useEffect(() => {
    let cancelled = false
    const load = () =>
      Promise.all([
        apiGet<TargetsConfigResponse>('/api/v1/config/targets'),
        apiGet<ProbesConfigResponse>('/api/v1/config/probes'),
        apiGet<AgentsResponse>('/api/v1/agents'),
        apiGet<OutagesResponse>('/api/v1/outages?window=24h'),
      ])
        .then(([targets, probes, agents, outages]) => {
          if (cancelled) return
          const probeSites = new Map<string, string[]>()
          for (const p of probes.probes) {
            // Direct probes carry a target name; mesh templates carry a
            // mesh. Disabled configs are not probing anyone — skip them.
            if (!p.target || !p.site || !p.enabled) continue
            const sites = probeSites.get(p.target) ?? []
            if (!sites.includes(p.site)) sites.push(p.site)
            probeSites.set(p.target, sites)
          }
          setFeeds({
            targets: targets.targets,
            probeSites,
            agentByID: new Map(agents.agents.map((a) => [a.id, a])),
            troubled: new Set(outages.outages.filter((o) => !o.closed_at && o.target).map((o) => o.target as string)),
          })
          setError('')
        })
        .catch((err) => {
          onAuthError(err)
          if (!cancelled) setError(err instanceof Error ? err.message : String(err))
        })
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError])

  const visible = useMemo(() => {
    if (!feeds) return { externals: [], sites: [] }
    const needle = query.trim().toLowerCase()
    const matches = (t: TargetConfig) => {
      if (!needle) return true
      const agent = agentIDOf(t) ? feeds.agentByID.get(agentIDOf(t) as string) : undefined
      return [t.name, t.address, t.url, agent?.site, agent?.hostname, ...(feeds.probeSites.get(t.name) ?? [])]
        .filter(Boolean)
        .some((v) => String(v).toLowerCase().includes(needle))
    }
    return {
      externals: feeds.targets.filter((t) => t.kind === 'external' && matches(t)),
      sites: feeds.targets.filter((t) => t.kind === 'agent' && matches(t)),
    }
  }, [feeds, query])

  if (error && !feeds)
    return (
      <div className="state-panel state-error">
        <h1>Targets unavailable</h1>
        <p>{error}</p>
      </div>
    )
  if (!feeds)
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading targets…
      </div>
    )

  const externalCount = feeds.targets.filter((t) => t.kind === 'external').length
  const siteCount = feeds.targets.length - externalCount
  const troubledCount = feeds.targets.filter((t) => feeds.troubled.has(t.name)).length

  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          <div className="eyebrow">Operations</div>
          <h1>Targets</h1>
          <p>Everything the fleet probes: external destinations and the sites' own agents.</p>
        </div>
        <div className="chips">
          <span className="chip">
            external <span className="mono">{externalCount}</span>
          </span>
          <span className="chip">
            site destinations <span className="mono">{siteCount}</span>
          </span>
          <span className="chip">
            {troubledCount > 0 && <span className="dot swatch status-down" />}
            with incidents <span className="mono">{troubledCount}</span>
          </span>
        </div>
      </div>

      {error && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}

      <div className="view-toolbar">
        <label className="search-field">
          <span className="sr-only">Search targets</span>
          <input
            type="search"
            placeholder="Search name, endpoint, or probing site"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
      </div>

      <div className="card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Probe destinations</span>
            <h2>External targets</h2>
          </div>
          <span className="hint">status reflects open incidents · admins manage these in Settings → Targets</span>
        </div>
        {visible.externals.length === 0 ? (
          <div className="empty-state">
            <strong>{query ? 'No matching external targets' : 'No external targets'}</strong>
            <span>
              {query
                ? 'Try a different name, endpoint, or probing site.'
                : 'An admin can add hosts and URLs to probe in Settings → Targets.'}
            </span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Name</th>
                  <th>Endpoint</th>
                  <th>Probes</th>
                  <th>Probed from</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {visible.externals.map((t) => (
                  <tr key={t.id}>
                    <td data-label="Status">
                      <StatusCell t={t} troubled={feeds.troubled} active={feeds.probeSites.has(t.name)} />
                    </td>
                    <td data-label="Name" className="mono">
                      <a href={'#/target/' + encodeURIComponent(t.id)}>{t.name}</a>
                    </td>
                    <td data-label="Endpoint" className="mono">
                      {t.url ? t.url : t.port ? `${t.address}:${t.port}` : t.address}
                    </td>
                    <td data-label="Probes">{t.probe_count}</td>
                    <td data-label="Probed from" className="mono">
                      {(feeds.probeSites.get(t.name) ?? []).join(', ') || '—'}
                    </td>
                    <td data-label="Created" title={fmtTime(t.created_at)}>
                      {fmtAgo(t.created_at)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Agent mesh</span>
            <h2>Site destinations</h2>
          </div>
          <span className="hint">each site's agent as a probe destination — inbound metrics per source site</span>
        </div>
        {visible.sites.length === 0 ? (
          <div className="empty-state">
            <strong>{query ? 'No matching site destinations' : 'No agents enrolled yet'}</strong>
            <span>
              {query ? 'Try a different site or hostname.' : 'Enroll an agent and its destination appears here.'}
            </span>
          </div>
        ) : (
          <div className="scroll-x">
            <table className="events">
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Site</th>
                  <th>Agent</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                {visible.sites.map((t) => {
                  const agent = agentIDOf(t) ? feeds.agentByID.get(agentIDOf(t) as string) : undefined
                  return (
                    <tr key={t.id}>
                      <td data-label="Status">
                        <StatusCell t={t} troubled={feeds.troubled} active />
                      </td>
                      <td data-label="Site" className="mono">
                        <a href={'#/target/' + encodeURIComponent(t.id)}>{agent ? agent.site : t.name}</a>
                      </td>
                      <td data-label="Agent" className="mono">
                        {agent ? agent.hostname : <span className="hint">deleted agent</span>}
                      </td>
                      <td data-label="Created" title={fmtTime(t.created_at)}>
                        {fmtAgo(t.created_at)}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}
