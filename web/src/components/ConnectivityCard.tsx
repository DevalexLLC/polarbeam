import MatrixTable from './MatrixTable'
import WorldMap from './WorldMap'
import type { ThresholdResolver } from '../severity'
import type { MatrixCell, MatrixResponse } from '../types'

export type ConnectivityMode = 'map' | 'matrix'

// The connectivity card owns the map/matrix switch in its own header (the
// retired Connectivity page's toolbar, moved in-card). Mode is lifted to the
// caller so other Overview elements — the "Healthy directions" tile — can
// flip straight to the matrix. cells may be a network-filtered subset of
// matrix.cells (the caller owns the filter state alongside the mode).
export default function ConnectivityCard({
  matrix,
  cells,
  networks,
  network,
  onNetworkChange,
  thresholds,
  mode,
  onModeChange,
}: {
  matrix: MatrixResponse
  cells: MatrixCell[]
  // Network filter options; the selector renders only when more than one
  // plane exists, so single-network installs keep the exact old header.
  networks: string[]
  network: string
  onNetworkChange: (network: string) => void
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
          {networks.length > 1 && (
            <label className="connectivity-network">
              <span className="sr-only">Network</span>
              <select value={network} onChange={(e) => onNetworkChange(e.target.value)}>
                <option value="">All networks</option>
                {networks.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
            </label>
          )}
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
        <WorldMap sites={matrix.sites} cells={cells} thresholds={thresholds} />
      ) : (
        <MatrixTable sites={matrix.sites} cells={cells} thresholds={thresholds} />
      )}
    </section>
  )
}
