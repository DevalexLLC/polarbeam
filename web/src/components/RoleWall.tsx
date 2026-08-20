import type { Caps } from '../caps'

// The panel-level guard, distinct from the settings nav's tab filtering.
// The two are independently correct on purpose: the nav decides what to
// OFFER, this decides what to FETCH. A panel keeps its own guard so it also
// skips a request that could only 403, and so a deep link or a role change
// mid-session lands on an explanation instead of an error.
//
// `need` reuses the Caps field names so the call site reads as a matched
// pair — `{!caps.adminWrite && <RoleWall need="adminWrite" …/>}` — and a
// mismatch is visible without leaving the line.
export default function RoleWall({
  need,
  what,
  caps,
}: {
  need: 'adminWrite' | 'networkWrite'
  what: string
  caps: Caps
}) {
  // A network_admin denied an adminWrite surface is not missing a role, it
  // is looking at something that spans every tenant. Saying "admin role
  // required" to an account whose role literally contains "admin" reads as
  // a bug, so name the reason instead.
  const spansTenants = need === 'adminWrite' && caps.networkWrite
  const title = spansTenants
    ? 'Deployment-wide setting'
    : need === 'adminWrite'
      ? 'Administrator role required'
      : 'Write access required'
  const body = spansTenants
    ? `${what} apply to every network and are managed by a global administrator. Your role manages the networks assigned to you.`
    : need === 'adminWrite'
      ? `${what} are visible to administrators only.`
      : `${what} are visible to administrators and network administrators only.`
  return (
    <div className="state-panel">
      <h2>{title}</h2>
      <p>{body}</p>
    </div>
  )
}
