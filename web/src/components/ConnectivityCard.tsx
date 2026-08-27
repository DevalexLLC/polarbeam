import MatrixTable from './MatrixTable'
import RadioButtonGroup from './RadioButtonGroup'
import TopologySites from './TopologySites'
import WorldMap from './WorldMap'
import type { ThresholdResolver } from '../severity'
import type { SiteTopology } from '../siteTopology'
import type { TopologyMode } from '../topologyMode'
import type { MatrixCell, MatrixResponse, Site } from '../types'

export type ConnectivityMode = TopologyMode

// The connectivity card owns the Sites/Map/Matrix switch in its own header (the
// retired Connectivity page's toolbar, moved in-card). Mode is lifted to the
// caller so other Overview elements — the "Healthy directions" tile — can
// flip straight to the matrix. The global top-bar network filter scopes this
// card through its props: sites and cells may be plane-filtered subsets of
// the matrix response (the caller derives both).
export default function ConnectivityCard({
  matrix,
  sites,
  cells,
  thresholds,
  topology,
  mode,
  onModeChange,
}: {
  matrix: MatrixResponse
  sites: Site[]
  cells: MatrixCell[]
  thresholds: ThresholdResolver
  topology: SiteTopology[]
  mode: ConnectivityMode
  onModeChange: (mode: ConnectivityMode) => void
}) {
  return (
    <section className="card overview-connectivity" id="connectivity">
      <div className="card-head">
        <div>
          <span className="eyebrow">Topology</span>
          <h2>Connectivity</h2>
        </div>
        <div className="card-head-actions">
          <span className="freshness">Latest {Math.round(matrix.horizon_s / 60)}-minute probe horizon</span>
          {/* One mode is always in effect, so this is a radiogroup, not
              independent toggles — arrows browse-and-select per the APG. */}
          <RadioButtonGroup
            label="Connectivity view"
            className="control-group"
            value={mode}
            options={[
              { value: 'sites', label: 'Sites' },
              { value: 'map', label: 'Map' },
              { value: 'matrix', label: 'Matrix' },
            ]}
            onChange={onModeChange}
          />
        </div>
      </div>
      {mode === 'sites' ? (
        <TopologySites topology={topology} />
      ) : mode === 'map' ? (
        <WorldMap topology={topology} />
      ) : (
        <MatrixTable sites={sites} cells={cells} thresholds={thresholds} />
      )}
    </section>
  )
}
