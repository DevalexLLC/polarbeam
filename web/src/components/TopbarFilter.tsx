import { useNetworkFilter } from '../networkFilter'

// Top-bar filter popover. One control scopes every view at once — the
// network is a property of what the operator is looking at, not of any one
// card, so it sits beside the other session-wide toggles. Network is the
// first dimension; the layout leaves room for more (site, probe type).
// Follows the user-menu <details> pattern: native summary toggling, items
// close the popover on activation.
export default function TopbarFilter({ networks }: { networks: string[] }) {
  const { network, setNetwork } = useNetworkFilter()
  // Single-network installs never filter: no control, top bar unchanged.
  if (networks.length <= 1) return null
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
        {['', ...networks].map((n) => (
          <button
            key={n}
            type="button"
            aria-pressed={network === n}
            onClick={(event) => {
              event.currentTarget.closest('details')?.removeAttribute('open')
              setNetwork(n)
            }}
          >
            {n === '' ? 'All networks' : n}
          </button>
        ))}
      </div>
    </details>
  )
}
