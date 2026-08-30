import { updateRouteParams } from '../routeState'

// The probes list's search box and route-backed Mode / State / Type
// filters. Everything here writes through the URL; the panel re-reads it.
export default function ProbesToolbar({
  query,
  onQueryChange,
  mode,
  enabled,
  typeFilter,
  types,
}: {
  query: string
  onQueryChange: (value: string) => void
  mode: string
  enabled: string
  typeFilter: string
  types: string[]
}) {
  return (
    <div className="view-toolbar data-table-toolbar">
      <label className="search-field">
        <span className="sr-only">Search probes</span>
        <input
          type="search"
          placeholder="Search assignment or type"
          value={query}
          onChange={(event) => onQueryChange(event.target.value)}
        />
      </label>
      <label className="compact-select">
        <span>Mode</span>
        <select
          value={mode}
          onChange={(event) => updateRouteParams({ mode: event.target.value, page: null, probe: null })}
        >
          <option value="all">All modes</option>
          <option value="direct">Direct</option>
          <option value="mesh">Mesh</option>
        </select>
      </label>
      <label className="compact-select">
        <span>State</span>
        <select
          value={enabled}
          onChange={(event) => updateRouteParams({ enabled: event.target.value, page: null, probe: null })}
        >
          <option value="all">All states</option>
          <option value="true">Enabled</option>
          <option value="false">Disabled</option>
        </select>
      </label>
      <label className="compact-select">
        <span>Type</span>
        <select
          value={typeFilter}
          onChange={(event) => updateRouteParams({ type: event.target.value, page: null, probe: null })}
        >
          <option value="all">All types</option>
          {types.map((item) => (
            <option key={item} value={item}>
              {item}
            </option>
          ))}
        </select>
      </label>
    </div>
  )
}
