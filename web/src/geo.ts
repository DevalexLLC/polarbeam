// Natural Earth I projection for the operations map. The raw polynomial is
// identical to d3-geo's naturalEarth1Raw and MUST stay in lockstep with
// web/tools/build-map-geo.mjs, which bakes the same projection into
// assets/mapGeo.ts — site positions and the committed dot grid share one
// transform so they can never drift.
import { MAP_K, MAP_TX, MAP_TY } from './assets/mapGeo'

const RAD = Math.PI / 180

function naturalEarth1Raw(lambda: number, phi: number): [number, number] {
  const phi2 = phi * phi
  const phi4 = phi2 * phi2
  return [
    lambda * (0.8707 - 0.131979 * phi2 + phi4 * (-0.013791 + phi4 * (0.003971 * phi2 - 0.001529 * phi4))),
    phi * (1.007226 + phi2 * (0.015085 + phi4 * (-0.044475 + 0.028874 * phi2 - 0.005916 * phi4))),
  ]
}

// Geographic degrees → map frame pixels (1080×600, fit constants from
// the generated asset; screen y grows downward).
export function projectMap(lon: number, lat: number): { x: number; y: number } {
  const [x, y] = naturalEarth1Raw(lon * RAD, lat * RAD)
  return { x: MAP_TX + MAP_K * x, y: MAP_TY - MAP_K * y }
}
