import { useNetworkFilter } from '../networkFilter'
import DisclosureMenu from './DisclosureMenu'
import RadioButtonGroup from './RadioButtonGroup'

// Top-bar filter popover. One control scopes every view at once — the
// network is a property of what the operator is looking at, not of any one
// card, so it sits beside the other session-wide toggles. Network is the
// first dimension; the layout leaves room for more (site, probe type).
// Shares the user menu's DisclosureMenu behavior; the mutually exclusive
// network choices are a radiogroup, so arrow keys browse-and-select while
// click/Enter also closes the popover.
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
    <DisclosureMenu
      className="topbar-filter"
      summaryLabel={network === '' ? 'Open filters' : `Filters: network ${network}`}
      summaryChildren={
        <>
          <svg className="topbar-filter-icon" viewBox="0 0 24 24" aria-hidden="true">
            <path d="M4 6h16M7.5 12h9M10.5 18h3" />
          </svg>
          {network !== '' && <span className="filter-badge" aria-hidden="true" />}
        </>
      }
    >
      <div className="topbar-filter-popover">
        <span className="topbar-filter-heading" aria-hidden="true">
          Network
        </span>
        <RadioButtonGroup
          label="Network filter"
          value={network}
          options={options.map((n) => ({
            value: n,
            label: n === '' ? (scope === null ? 'All networks' : 'All my networks') : n,
          }))}
          onChange={setNetwork}
          onActivate={(_, event) => {
            event.currentTarget.closest('details')?.removeAttribute('open')
          }}
        />
      </div>
    </DisclosureMenu>
  )
}
