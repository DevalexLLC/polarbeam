import { directionSeverity, SEVERITY_LABEL, type Severity, type ThresholdResolver } from '../severity'
import type { MatrixCell, Site } from '../types'
import { fmtLatency } from '../format'

// Matrix cells grade through the same directionSeverity fold as the map and
// Overview — the raw API status alone would call a threshold-violating
// direction "Healthy" while the other views show it Degraded. Severity →
// cell visual class: warn and crit both render the shared "Degraded"
// treatment (crit's stronger intensity lives in detailed views).
type CellClass = 'ok' | 'degraded' | 'down' | 'stale'
export const SEV_CLASS: Record<Severity, CellClass> = {
  ok: 'ok',
  warn: 'degraded',
  crit: 'degraded',
  down: 'down',
  stale: 'stale',
}

// Status is never conveyed by color alone: cells carry the status word or a
// latency figure, and the legend pairs every swatch with its label.
export const CLASS_LABEL: Record<CellClass, string> = {
  ok: SEVERITY_LABEL.ok,
  degraded: SEVERITY_LABEL.warn,
  down: SEVERITY_LABEL.down,
  stale: SEVERITY_LABEL.stale,
}

function Cell({ cell, thresholds }: { cell: MatrixCell; thresholds: ThresholdResolver }) {
  const cls = SEV_CLASS[directionSeverity(cell, thresholds(cell.src, cell.dst))]
  const failed = cell.probes.filter((p) => p.status !== 'ok').length
  const total = cell.probes.length
  const detail = cell.probes
    .map(
      (p) =>
        `${p.type}: ${p.status}` +
        `${p.latency_us != null ? ` · ${fmtLatency(p.latency_us)}` : ''}` +
        `${p.loss_pct != null && p.loss_pct > 0 ? ` · ${p.loss_pct.toFixed(0)}% loss` : ''}`,
    )
    .join(', ')
  const hasLatency = cell.status === 'ok' || cell.status === 'degraded'
  // Compact labels so a full mesh fits the Overview card without scroll;
  // the title/aria keep the long form.
  const checks =
    cell.status === 'stale'
      ? 'No data'
      : cell.status === 'ok'
        ? `${total}/${total} healthy`
        : `${failed}/${total} failed`
  // The API intentionally reports the best working latency and the worst
  // probe loss. Label that fold explicitly so mixed checks never read as a
  // single direction simultaneously succeeding and losing every packet.
  const worstLoss = cell.loss_pct != null && cell.loss_pct > 0 ? ` · worst ${cell.loss_pct.toFixed(0)}% loss` : ''
  const checksLong =
    cell.status === 'stale'
      ? 'No recent data'
      : cell.status === 'ok'
        ? `${total} of ${total} checks healthy`
        : `${failed} of ${total} checks failed`
  const worstLossLong =
    cell.loss_pct != null && cell.loss_pct > 0 ? `, worst probe ${cell.loss_pct.toFixed(0)}% loss` : ''
  return (
    <td className={'cell status-' + cls}>
      <a
        href={`#/pair/${encodeURIComponent(cell.src)}/${encodeURIComponent(cell.dst)}`}
        title={`${cell.src} → ${cell.dst} · ${CLASS_LABEL[cls]} · ${detail}`}
        aria-label={`${cell.src} to ${cell.dst}: ${CLASS_LABEL[cls]}. ${checksLong}${worstLossLong}. ${detail}`}
      >
        <span className="cell-status">
          <span className={'dot swatch status-' + cls} />
          {CLASS_LABEL[cls]}
        </span>
        <span className="cell-value">{hasLatency ? fmtLatency(cell.latency_us) : '—'}</span>
        <span className="cell-sub">
          {checks}
          {worstLoss}
        </span>
      </a>
    </td>
  )
}

// The directional matrix table, shared verbatim from the retired
// Connectivity page: rows are sources, columns destinations, cells link to
// pair detail.
export default function MatrixTable({
  sites,
  cells,
  thresholds,
}: {
  sites: Site[]
  cells: MatrixCell[]
  thresholds: ThresholdResolver
}) {
  // NUL separator: site names are unrestricted text and NUL cannot appear
  // in Postgres text (same convention as WorldMap's pairKey).
  const cellFor = new Map<string, MatrixCell>()
  for (const c of cells) cellFor.set(c.src + '\u0000' + c.dst, c)

  if (sites.length < 2) {
    return (
      <p className="muted">
        Fewer than two sites are enrolled. Enroll agents at a second site and add both to a mesh group to light up the
        board.
      </p>
    )
  }

  return (
    <>
      <div className="scroll-x">
        <table className="matrix">
          <thead>
            <tr>
              <th className="corner eyebrow" scope="col">
                source ↓<br />
                destination →
              </th>
              {sites.map((s) => (
                <th key={s.name} scope="col">
                  {s.display_name || s.name}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {sites.map((src) => (
              <tr key={src.name}>
                <th scope="row">{src.display_name || src.name}</th>
                {sites.map((dst) => {
                  if (src.name === dst.name) return <td key={dst.name} className="diag" aria-label="same site" />
                  const cell = cellFor.get(src.name + '\u0000' + dst.name)
                  if (!cell)
                    return (
                      <td key={dst.name} className="empty">
                        not probed
                      </td>
                    )
                  return <Cell key={dst.name} cell={cell} thresholds={thresholds} />
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="legend">
        {(['ok', 'degraded', 'down', 'stale'] as const).map((s) => (
          <span key={s} className="legend-item">
            <span className={'swatch status-' + s} /> {CLASS_LABEL[s]}
          </span>
        ))}
      </div>
    </>
  )
}
