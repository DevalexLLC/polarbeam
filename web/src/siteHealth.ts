// Site-level health vocabulary shared by the Overview tiles and the site
// page, kept pure so both derive the same answer from the same rows.
import type { AgentInfo } from './types'

// An agent counts as live when it has reported, is not offline, and holds
// a usable certificate — the "sites available" predicate.
export function agentIsLive(a: AgentInfo): boolean {
  return (
    a.last_seen_at != null &&
    !a.offline &&
    !a.cert_revoked_at &&
    (a.cert_not_after == null || Date.parse(a.cert_not_after) >= Date.now())
  )
}

// Stat-tile tone for a "value / total" ratio: all good, none good, or mixed;
// no tone at all when there is nothing to grade.
export function ratioStatus(value: number, total: number): string {
  if (total === 0) return ''
  if (total > 0 && value === total) return ' stat-good'
  if (total > 0 && value === 0) return ' stat-critical'
  return ' stat-warning'
}
