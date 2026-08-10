import MatrixTable from './MatrixTable'
import WorldMap from './WorldMap'
import type { MatrixResponse, ThresholdSettings } from '../types'

export type ConnectivityMode = 'map' | 'matrix'

// The connectivity card owns the map/matrix switch in its own header (the
// retired Connectivity page's toolbar, moved in-card). Mode is lifted to the
// caller so other Overview elements — the "Healthy directions" tile — can
// flip straight to the matrix.
export default function ConnectivityCard({
  matrix,
  thresholds,
  mode,
  onModeChange,
}: {
  matrix: MatrixResponse
  thresholds: ThresholdSettings | null
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
        <WorldMap sites={matrix.sites} cells={matrix.cells} thresholds={thresholds} />
      ) : (
        <MatrixTable sites={matrix.sites} cells={matrix.cells} thresholds={thresholds} />
      )}
    </section>
  )
}
