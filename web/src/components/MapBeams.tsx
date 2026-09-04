import { memo, useMemo } from 'react'
import { beamGeometry, type BeamEnd, type BeamSegment, type SiteLink } from '../mapLinks'
import type { Severity } from '../severity'

const SEVERITIES: Severity[] = ['ok', 'warn', 'crit', 'down', 'stale']

// One arrowhead marker per severity: markers render in their own context
// and cannot read the stroke color of the path that references them, so
// each carries its own class and the stylesheet colors it to match.
export function BeamMarkers({ id }: { id: string }) {
  return (
    <defs>
      {SEVERITIES.map((sev) => (
        <marker
          key={sev}
          id={`${id}-${sev}`}
          className={`map-beam-arrow sev-${sev}`}
          markerWidth="4"
          markerHeight="4"
          refX="4"
          refY="2"
          orient="auto"
          markerUnits="strokeWidth"
        >
          <path d="M0 0 L4 2 L0 4 Z" />
        </marker>
      ))}
    </defs>
  )
}

interface PlacedLink {
  key: string
  a: string
  b: string
  beams: { side: 1 | -1; severity: Severity; seg: BeamSegment }[]
}

// Directional strokes under the site bubbles. Decorative under the map's
// role="img": every direction's severity is already carried by the site
// info card's counts, and the matrix view lists them one by one. `lit`
// names a site whose beams stay full-strength while the rest recede.
//
// A full mesh is quadratic in sites, and the parent re-renders on every
// pan, zoom, and hover frame, so the geometry is computed only when the
// links or endpoints change and the component skips renders whose props
// are unchanged; a hover re-render only toggles a class per pair.
function MapBeams({
  links,
  ends,
  markerId,
  lit,
}: {
  links: SiteLink[]
  ends: ReadonlyMap<string, BeamEnd>
  markerId: string
  lit: string | null
}) {
  const placed = useMemo<PlacedLink[]>(() => {
    const out: PlacedLink[] = []
    for (const link of links) {
      const from = ends.get(link.a)
      const to = ends.get(link.b)
      if (!from || !to) continue
      const beams: PlacedLink['beams'] = []
      for (const [severity, side] of [
        [link.ab, 1],
        [link.ba, -1],
      ] as const) {
        if (severity === null) continue
        const seg = beamGeometry(from, to, side)
        if (seg) beams.push({ side, severity, seg })
      }
      if (beams.length > 0) out.push({ key: link.a + '\u0000' + link.b, a: link.a, b: link.b, beams })
    }
    return out
  }, [links, ends])

  return (
    <g className="map-beams" aria-hidden="true">
      {placed.map((link) => (
        <g key={link.key} className={lit === link.a || lit === link.b ? 'lit' : undefined}>
          {link.beams.map(({ side, severity, seg }) => (
            <line
              key={side}
              className={`map-beam sev-${severity}`}
              x1={seg.x1}
              y1={seg.y1}
              x2={seg.x2}
              y2={seg.y2}
              markerEnd={`url(#${markerId}-${severity})`}
            />
          ))}
        </g>
      ))}
    </g>
  )
}

export default memo(MapBeams)
