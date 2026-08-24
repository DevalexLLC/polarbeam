export const TOPOLOGY_MODES = ['sites', 'map', 'matrix'] as const

export type TopologyMode = (typeof TOPOLOGY_MODES)[number]

export function isTopologyMode(value: string): value is TopologyMode {
  return TOPOLOGY_MODES.includes(value as TopologyMode)
}

// An omitted mode is deliberately responsive. Once the operator chooses a
// mode it is explicit in the URL, so copied links behave identically at every
// viewport width.
export function resolveTopologyMode(explicit: string, narrow: boolean): TopologyMode {
  return isTopologyMode(explicit) ? explicit : narrow ? 'sites' : 'map'
}
