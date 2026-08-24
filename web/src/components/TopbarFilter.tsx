import { useNetworkFilter } from '../networkFilter'

// Top-bar filter popover. One control scopes every view at once — the
// network is a property of what the operator is looking at, not of any one
// card, so it sits beside the other session-wide toggles. Network is the
// first dimension; the layout leaves room for more (site, probe type).
// Follows the user-menu <details> pattern: native summary toggling, items
// close the popover on activation.
export default function TopbarFilter({ networks, scope }: { networks: string[]; scope: string[] | null }) {
  const { network, setNetwork } = useNetworkFilter()
  // A scoped account limited to exactly one plane has nothing to filter —
  // the server already returns that plane alone — but it should still be
  // able to SEE which one it is looking at, so the control becomes a static
  // chip instead of disappearing.
  if (scope !== null && networks.length === 1) {
    return <span className="chip topbar-plane">{networks[0]}</span>
  }
  // Single-network installs never filter: no control, top bar unchanged.
  // But an ACTIVE filter always keeps the control (and "All networks")
  // reachable — a URL filter applies before the networks list loads,
  // and if that fetch fails the user must still be able to clear it.
  if (networks.length <= 1 && network === '') return null
  const options = ['', ...networks]
  if (network !== '' && !networks.includes(network)) options.push(network)
  return (
    <details className="topbar-filter">
      <summary aria-label={network === '' ? 'Open filters' : `Filters: network ${network}`}>
        <svg className="topbar-filter-icon" viewBox="0 0 24 24" aria-hidden="true">
          <path d="M4 6h16M7.5 12h9M10.5 18h3" />
        </svg>
        {network !== '' && <span className="filter-badge" aria-hidden="true" />}
      </summary>
      <div className="topbar-filter-popover">
        <span className="topbar-filter-heading">Network</span>
        {options.map((n) => (
          <button
            key={n}
            type="button"
            aria-pressed={network === n}
            onClick={(event) => {
              event.currentTarget.closest('details')?.removeAttribute('open')
              setNetwork(n)
            }}
          >
            {n === '' ? (scope === null ? 'All networks' : 'All my networks') : n}
          </button>
        ))}
      </div>
    </details>
  )
}
