export type HistoryMode = 'push' | 'replace'

export interface CanonicalRouteState {
  hash: string
  path: string
  params: URLSearchParams
  changed: boolean
}

export interface CanonicalRouteOptions {
  knownNetworks?: readonly string[]
  settingsSections?: readonly string[]
}

const WINDOWS = ['24h', '7d', '30d', '90d', '365d'] as const
const SETTINGS_SECTIONS = [
  'thresholds',
  'sites',
  'networks',
  'targets',
  'meshes',
  'probes',
  'enrollment',
  'users',
  'authentication',
  'banner',
] as const

const ROUTE_STATE_EVENT = 'polarbeam-route-state'

function decodeSegment(value: string): string {
  try {
    return decodeURIComponent(value)
  } catch {
    return value
  }
}

function encodedSegment(value: string): string {
  return encodeURIComponent(decodeSegment(value))
}

function oneOf(params: URLSearchParams, name: string, allowed: readonly string[], fallback: string): string {
  const value = params.get(name)
  return value !== null && allowed.includes(value) ? value : fallback
}

function opaque(params: URLSearchParams, name: string): string {
  return params.get(name) ?? ''
}

function positiveInteger(params: URLSearchParams, name: string, fallback: number): number {
  const raw = params.get(name)
  if (raw === null || !/^[1-9]\d*$/.test(raw)) return fallback
  const value = Number(raw)
  return Number.isSafeInteger(value) ? value : fallback
}

function setNonDefault(out: URLSearchParams, name: string, value: string, fallback = '') {
  if (value !== fallback) out.set(name, value)
}

function normalizePath(rawPath: string, source: URLSearchParams): string {
  const parts = rawPath.replace(/^\/?/, '').split('/').filter(Boolean)
  const head = parts[0] ?? ''
  if (head === '' || head === 'connectivity' || head === 'sightlines') return '/'
  if (head === 'outages' || head === 'incidents') return '/incidents'
  if (head === 'paths' || head === 'routes') return '/routes'
  if (head === 'targets') return '/targets'
  if (head === 'agents') {
    if (parts[1] && !source.has('agent')) source.set('agent', decodeSegment(parts[1]))
    return '/agents'
  }
  if (head === 'pair' && parts[1] && parts[2]) return `/pair/${encodedSegment(parts[1])}/${encodedSegment(parts[2])}`
  if (head === 'target') return parts[1] ? `/target/${encodedSegment(parts[1])}` : '/target'
  if (head === 'settings') {
    if (parts[1] && !source.has('section')) source.set('section', decodeSegment(parts[1]))
    return '/settings'
  }
  if (/^sso-error=[a-z-]+$/.test(head)) return `/${head}`
  if (head === 'about') return '/about'
  return `/${parts.map(encodedSegment).join('/')}`
}

function routeName(path: string): string {
  if (path.startsWith('/pair/')) return 'pair'
  if (path.startsWith('/target/')) return 'target'
  return path.slice(1) || 'overview'
}

// Canonicalize the query inside a hash route. The fixed insertion order is
// intentional: equivalent state has one copyable URL, which also prevents
// replaceState loops when a bookmark contains reordered or duplicate keys.
export function canonicalizeRouteHash(hash: string, options: CanonicalRouteOptions = {}): CanonicalRouteState {
  const raw = hash.replace(/^#/, '') || '/'
  const queryAt = raw.indexOf('?')
  const rawPath = queryAt === -1 ? raw : raw.slice(0, queryAt)
  const source = new URLSearchParams(queryAt === -1 ? '' : raw.slice(queryAt + 1))
  const path = normalizePath(rawPath, source)
  const route = routeName(path)
  const out = new URLSearchParams()

  const requestedNetwork = opaque(source, 'network')
  const known = options.knownNetworks
  if (requestedNetwork && (known === undefined || (known.length > 1 && known.includes(requestedNetwork)))) {
    out.set('network', requestedNetwork)
  }

  if (route === 'overview') {
    setNonDefault(out, 'topology', oneOf(source, 'topology', ['map', 'matrix'], 'map'), 'map')
  } else if (route === 'incidents') {
    setNonDefault(out, 'window', oneOf(source, 'window', WINDOWS, '24h'), '24h')
    setNonDefault(out, 'status', oneOf(source, 'status', ['active', 'all', 'resolved'], 'active'), 'active')
    setNonDefault(out, 'q', opaque(source, 'q'))
    const slice = positiveInteger(source, 'slice', 0)
    if (slice > 0) out.set('slice', String(slice))
    setNonDefault(out, 'incident', opaque(source, 'incident'))
  } else if (route === 'routes') {
    setNonDefault(out, 'window', oneOf(source, 'window', WINDOWS, '24h'), '24h')
    setNonDefault(out, 'q', opaque(source, 'q'))
    setNonDefault(out, 'sort', oneOf(source, 'sort', ['time', 'source', 'destination', 'changes'], 'time'), 'time')
    setNonDefault(out, 'order', oneOf(source, 'order', ['asc', 'desc'], 'desc'), 'desc')
    const page = positiveInteger(source, 'page', 1)
    if (page > 1) out.set('page', String(page))
    setNonDefault(out, 'event', opaque(source, 'event'))
  } else if (route === 'targets') {
    setNonDefault(out, 'q', opaque(source, 'q'))
    setNonDefault(out, 'kind', oneOf(source, 'kind', ['all', 'external', 'agent'], 'all'), 'all')
    setNonDefault(out, 'status', oneOf(source, 'status', ['all', 'incident', 'healthy', 'unprobed'], 'all'), 'all')
    setNonDefault(out, 'sort', oneOf(source, 'sort', ['name', 'status', 'probes', 'created'], 'name'), 'name')
    setNonDefault(out, 'order', oneOf(source, 'order', ['asc', 'desc'], 'asc'), 'asc')
    const page = positiveInteger(source, 'page', 1)
    if (page > 1) out.set('page', String(page))
  } else if (route === 'agents') {
    setNonDefault(out, 'q', opaque(source, 'q'))
    setNonDefault(out, 'health', oneOf(source, 'health', ['all', 'attention', 'healthy'], 'all'), 'all')
    setNonDefault(out, 'sort', oneOf(source, 'sort', ['status', 'site', 'hostname', 'last_seen'], 'status'), 'status')
    setNonDefault(out, 'order', oneOf(source, 'order', ['asc', 'desc'], 'asc'), 'asc')
    const page = positiveInteger(source, 'page', 1)
    if (page > 1) out.set('page', String(page))
    setNonDefault(out, 'agent', opaque(source, 'agent'))
    setNonDefault(out, 'probe', opaque(source, 'probe'))
  } else if (route === 'pair' || route === 'target') {
    setNonDefault(out, 'window', oneOf(source, 'window', WINDOWS, '24h'), '24h')
    setNonDefault(out, 'metric', oneOf(source, 'metric', ['latency', 'loss'], 'latency'), 'latency')
    if (route === 'target') {
      setNonDefault(out, 'probe', opaque(source, 'probe'))
      setNonDefault(out, 'from', opaque(source, 'from'))
    }
  } else if (route === 'settings') {
    const sections = options.settingsSections ?? SETTINGS_SECTIONS
    const section = oneOf(source, 'section', sections, sections[0] ?? '')
    setNonDefault(out, 'section', section, sections[0] ?? '')
    setNonDefault(out, 'subsection', opaque(source, 'subsection'))
    if (section === 'sites') setNonDefault(out, 'site', opaque(source, 'site'))
    if (section === 'probes') setNonDefault(out, 'probe', opaque(source, 'probe'))
  }

  const query = out.toString()
  const canonical = `#${path}${query ? `?${query}` : ''}`
  return { hash: canonical, path, params: out, changed: canonical !== (hash || '#/') }
}

export function routeParam(hash: string, name: string, options?: CanonicalRouteOptions): string {
  return canonicalizeRouteHash(hash, options).params.get(name) ?? ''
}

export function routeNumberParam(
  hash: string,
  name: string,
  fallback: number,
  options?: CanonicalRouteOptions,
): number {
  const raw = routeParam(hash, name, options)
  if (!/^[1-9]\d*$/.test(raw)) return fallback
  const value = Number(raw)
  return Number.isSafeInteger(value) ? value : fallback
}

// Internal navigation keeps the active plane explicit in its destination.
// This preserves the operator's scope without an out-of-URL singleton, and
// makes context-menu copies and new-tab navigation as correct as clicks.
export function inheritRouteNetwork(href: string, sourceHash?: string): string {
  if (!href.startsWith('#/')) return href
  const target = canonicalizeRouteHash(href)
  const network = canonicalizeRouteHash(sourceHash ?? routeHashSnapshot()).params.get('network')
  if (!network || target.params.has('network')) return target.hash
  const params = new URLSearchParams(target.params)
  params.set('network', network)
  return canonicalizeRouteHash(`#${target.path}?${params.toString()}`).hash
}

// Incident investigation links retain the selected plane and window, and
// carry the stable route-event identity that the Routes view expands.
export function routeEventHref(eventID: string, window: string, sourceHash = routeHashSnapshot()): string {
  const params = new URLSearchParams()
  if (window !== '24h') params.set('window', window)
  params.set('event', eventID)
  return inheritRouteNetwork(`#/routes?${params.toString()}`, sourceHash)
}

export function incidentAgentHref(
  agentID: string,
  probeID: string | null,
  liveLabel: string,
  sourceHash = routeHashSnapshot(),
): string | null {
  if (!agentID || !liveLabel) return null
  const params = new URLSearchParams({ agent: agentID })
  if (probeID) params.set('probe', probeID)
  return inheritRouteNetwork(`#/agents?${params.toString()}`, sourceHash)
}

export function incidentPairHref(
  source: string,
  destination: string | null,
  window: string,
  sourceHash = routeHashSnapshot(),
): string | null {
  if (!source || !destination) return null
  const query = window === '24h' ? '' : `?window=${encodeURIComponent(window)}`
  return inheritRouteNetwork(
    `#/pair/${encodeURIComponent(source)}/${encodeURIComponent(destination)}${query}`,
    sourceHash,
  )
}

export function incidentTargetHref(
  targetID: string | null,
  probeID: string | null,
  liveLabel: string | null,
  window: string,
  sourceHash = routeHashSnapshot(),
): string | null {
  if (!targetID || !liveLabel) return null
  const params = new URLSearchParams()
  if (window !== '24h') params.set('window', window)
  if (probeID) params.set('probe', probeID)
  const query = params.toString()
  return inheritRouteNetwork(`#/target/${encodeURIComponent(targetID)}${query ? `?${query}` : ''}`, sourceHash)
}

// Inventory links carry their canonical URL state into target detail so the
// breadcrumb works in copied/new-tab links as well as browser history. Other
// entry points fall back to the Targets landing page in the current plane.
export function targetDetailHref(id: string, sourceHash = routeHashSnapshot()): string {
  const source = canonicalizeRouteHash(sourceHash)
  const target = canonicalizeRouteHash(`#/target/${encodeURIComponent(id)}`)
  const params = new URLSearchParams(target.params)
  const network = source.params.get('network')
  if (network) params.set('network', network)
  if (source.path === '/targets') params.set('from', source.hash)
  return canonicalizeRouteHash(`#${target.path}?${params.toString()}`).hash
}

export function targetInventoryHref(sourceHash = routeHashSnapshot()): string {
  const source = canonicalizeRouteHash(sourceHash)
  const from = source.params.get('from')
  if (from) {
    const inventory = canonicalizeRouteHash(from)
    if (inventory.path === '/targets') {
      const params = new URLSearchParams(inventory.params)
      const currentNetwork = source.params.get('network')
      if (currentNetwork) params.set('network', currentNetwork)
      else params.delete('network')
      return canonicalizeRouteHash(`#${inventory.path}?${params.toString()}`).hash
    }
  }
  return inheritRouteNetwork('#/targets', source.hash)
}

export function updateRouteParams(
  updates: Record<string, string | number | null | undefined>,
  mode: HistoryMode = 'push',
): string {
  const current = canonicalizeRouteHash(location.hash)
  const params = new URLSearchParams(current.params)
  for (const [name, value] of Object.entries(updates)) {
    if (value === null || value === undefined || value === '') params.delete(name)
    else params.set(name, String(value))
  }
  const query = params.toString()
  const next = canonicalizeRouteHash(`#${current.path}${query ? `?${query}` : ''}`).hash
  if (next === current.hash && location.hash === next) return next
  history[mode === 'replace' ? 'replaceState' : 'pushState'](null, '', next)
  window.dispatchEvent(new Event(ROUTE_STATE_EVENT))
  return next
}

export function replaceCanonicalRoute(options: CanonicalRouteOptions = {}, notify = true): string {
  const canonical = canonicalizeRouteHash(location.hash, options)
  if (!canonical.changed) return canonical.hash
  history.replaceState(null, '', canonical.hash)
  if (notify) window.dispatchEvent(new Event(ROUTE_STATE_EVENT))
  return canonical.hash
}

export function subscribeRouteState(listener: () => void): () => void {
  window.addEventListener('hashchange', listener)
  window.addEventListener('popstate', listener)
  window.addEventListener(ROUTE_STATE_EVENT, listener)
  return () => {
    window.removeEventListener('hashchange', listener)
    window.removeEventListener('popstate', listener)
    window.removeEventListener(ROUTE_STATE_EVENT, listener)
  }
}

export function routeHashSnapshot(): string {
  return location.hash || '#/'
}
