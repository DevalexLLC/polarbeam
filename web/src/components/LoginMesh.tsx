import { useId } from 'react'
import MapBeams, { BeamMarkers } from './MapBeams'
import { projectMap } from '../geo'
import { DOT_GRID_D } from '../mapDots'
import { bubbleRadius } from '../mapLayout'
import type { BeamEnd, SiteLink } from '../mapLinks'
import type { Severity } from '../severity'

// A still of the operations map with an illustrative five-site mesh: the
// same world, bubbles, and directional beams the dashboard draws, so the
// sign-in page shows the product rather than describing it. Static and
// decorative — the caption beside it carries the words.
// An Atlantic mesh: the panel is portrait, so the sample sites span more
// latitude than longitude and the whole mesh stays inside the crop.
const SAMPLE_SITES: { name: string; lon: number; lat: number; severity: Severity }[] = [
  { name: 'den', lon: -104.99, lat: 39.74, severity: 'ok' },
  { name: 'nyc', lon: -74.01, lat: 40.71, severity: 'ok' },
  { name: 'lon', lon: -0.13, lat: 51.51, severity: 'warn' },
  { name: 'fra', lon: 8.68, lat: 50.11, severity: 'ok' },
  { name: 'sao', lon: -46.63, lat: -23.55, severity: 'ok' },
]

// Full mesh, every direction healthy except the one the London bubble
// reports: what a single degraded direction looks like on the map.
const SAMPLE_LINKS: SiteLink[] = []
for (let i = 0; i < SAMPLE_SITES.length; i++) {
  for (let j = i + 1; j < SAMPLE_SITES.length; j++) {
    const [a, b] =
      SAMPLE_SITES[i].name < SAMPLE_SITES[j].name
        ? [SAMPLE_SITES[i].name, SAMPLE_SITES[j].name]
        : [SAMPLE_SITES[j].name, SAMPLE_SITES[i].name]
    SAMPLE_LINKS.push({ a, b, ab: a === 'lon' && b === 'nyc' ? 'warn' : 'ok', ba: 'ok' })
  }
}

const PLACED = SAMPLE_SITES.map((site) => ({ ...site, ...projectMap(site.lon, site.lat) }))
const ENDS = new Map<string, BeamEnd>(PLACED.map((p) => [p.name, { x: p.x, y: p.y, r: bubbleRadius(4) }]))
// A portrait frame around the mesh (the panel is taller than wide) with
// room at the edges; the svg covers its panel, so a wider panel trims a
// little from the top and bottom and a narrower one from the sides.
const FRAME_ASPECT = 0.9
const FRAME_FILL = 0.68
const VIEW = (() => {
  const xs = PLACED.map((p) => p.x)
  const ys = PLACED.map((p) => p.y)
  const [minX, maxX, minY, maxY] = [Math.min(...xs), Math.max(...xs), Math.min(...ys), Math.max(...ys)]
  const width = Math.max((maxX - minX) / FRAME_FILL, ((maxY - minY) * FRAME_ASPECT) / FRAME_FILL)
  const height = width / FRAME_ASPECT
  return { x: (minX + maxX) / 2 - width / 2, y: (minY + maxY) / 2 - height / 2, width, height }
})()

export default function LoginMesh() {
  const markerId = useId()
  return (
    <svg
      className="login-mesh"
      viewBox={`${VIEW.x} ${VIEW.y} ${VIEW.width} ${VIEW.height}`}
      preserveAspectRatio="xMidYMid slice"
      aria-hidden="true"
      focusable="false"
    >
      <BeamMarkers id={markerId} />
      <path className="map-dotgrid" d={DOT_GRID_D} />
      <MapBeams links={SAMPLE_LINKS} ends={ENDS} markerId={markerId} lit={null} />
      {PLACED.map((p) => (
        <g key={p.name} className={`map-site sev-${p.severity}`}>
          <circle className="map-bubble" cx={p.x} cy={p.y} r={bubbleRadius(4)} />
          <circle className="map-bubble-core" cx={p.x} cy={p.y} r={3} />
        </g>
      ))}
    </svg>
  )
}
