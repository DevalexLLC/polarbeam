import MatrixTable from './MatrixTable'
import WorldMap from './WorldMap'
import type { ThresholdResolver } from '../severity'
import type { MatrixCell, MatrixResponse, Site } from '../types'

export type ConnectivityMode = 'map' | 'matrix'

// The connectivity card owns the map/matrix switch in its own header (the
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
  mode,
  onModeChange,
}: {
  matrix: MatrixResponse
  sites: Site[]
  cells: MatrixCell[]
  thresholds: ThresholdResolver
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
          <div className="control-group" role="group" aria-label="Connectivity view">
            <button
              className={mode === 'map' ? 'active' : ''}
              aria-pressed={mode === 'map'}
              onClick={() => onModeChange('map')}
            >
              Map
            </button>
            <button
              className={mode === 'matrix' ? 'active' : ''}
              aria-pressed={mode === 'matrix'}
              onClick={() => onModeChange('matrix')}
            >
              Matrix
            </button>
          </div>
        </div>
      </div>
      {mode === 'map' ? (
        <WorldMap sites={sites} cells={cells} thresholds={thresholds} />
      ) : (
        <MatrixTable sites={sites} cells={cells} thresholds={thresholds} />
      )}
    </section>
  )
}
