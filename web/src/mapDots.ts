import { MAP_DOTS } from './assets/mapGeo'

// The dot-matrix landmass: one path of zero-length round-capped segments,
// computed once — the geometry never changes at runtime. Shared by the
// operations map and the sign-in illustration so both draw the same world.
let dotGrid = ''
for (let i = 0; i < MAP_DOTS.length; i += 2) {
  dotGrid += `M${MAP_DOTS[i]} ${MAP_DOTS[i + 1]}h.01`
}
export const DOT_GRID_D = dotGrid
