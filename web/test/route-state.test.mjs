import assert from 'node:assert/strict'
import test from 'node:test'
import {
  canonicalizeRouteHash,
  incidentAgentHref,
  incidentPairHref,
  incidentTargetHref,
  inheritRouteNetwork,
  routeNumberParam,
  routeParam,
  routeEventHref,
  routeChangeDiscardsSettingsDraft,
  setRouteNavigationBlocker,
  subscribeRouteState,
  targetDetailHref,
  targetInventoryHref,
  updateRouteParams,
} from '../src/routeState.ts'

test('canonical state omits defaults, unknown keys, and invalid values', () => {
  assert.equal(
    canonicalizeRouteHash('#/incidents?window=24h&status=nope&page=3&q=router&network=blue&junk=1').hash,
    '#/incidents?network=blue&q=router',
  )
  assert.equal(canonicalizeRouteHash('#/routes?page=01&sort=nope&order=asc').hash, '#/routes?order=asc')
  assert.equal(canonicalizeRouteHash('#/target/a%2Fb?metric=nope&window=7d').hash, '#/target/a%2Fb?window=7d')
})

test('legacy route paths canonicalize without losing selection', () => {
  assert.equal(canonicalizeRouteHash('#/outages?status=resolved').hash, '#/incidents?status=resolved')
  assert.equal(canonicalizeRouteHash('#/paths?q=ny').hash, '#/routes?q=ny')
  assert.equal(canonicalizeRouteHash('#/agents/a%2Fb').hash, '#/agents?agent=a%2Fb')
  assert.equal(canonicalizeRouteHash('#/settings/probes').hash, '#/settings?section=monitoring&subsection=probes')
})

test('unknown and malformed target paths remain recoverable routes', () => {
  assert.equal(canonicalizeRouteHash('#/does-not-exist?junk=1').hash, '#/does-not-exist')
  assert.equal(canonicalizeRouteHash('#/target').hash, '#/target')
  assert.equal(canonicalizeRouteHash('#/target/%').hash, '#/target/%25')
  assert.equal(canonicalizeRouteHash('#/sso-error=config').hash, '#/sso-error=config')
})

test('network is URL-only and reconciles against accessible planes', () => {
  const hash = '#/agents?network=secret&health=attention'
  assert.equal(canonicalizeRouteHash(hash).hash, hash)
  assert.equal(canonicalizeRouteHash(hash, { knownNetworks: ['blue', 'green'] }).hash, '#/agents?health=attention')
  assert.equal(canonicalizeRouteHash('#/agents?network=blue', { knownNetworks: ['blue'] }).hash, '#/agents')
  assert.equal(
    inheritRouteNetwork('#/incidents?status=resolved', '#/agents?network=blue'),
    '#/incidents?network=blue&status=resolved',
  )
  assert.equal(
    inheritRouteNetwork('#/target/t1?probe=p1', '#/agents?network=blue'),
    '#/target/t1?network=blue&probe=p1',
  )
})

test('route schemas preserve each supported view state in stable order', () => {
  assert.equal(canonicalizeRouteHash('#/?topology=matrix&network=blue').hash, '#/?network=blue&topology=matrix')
  assert.equal(canonicalizeRouteHash('#/?topology=sites').hash, '#/?topology=sites')
  assert.equal(canonicalizeRouteHash('#/?topology=map').hash, '#/?topology=map')
  assert.equal(canonicalizeRouteHash('#/?topology=invalid').hash, '#/')
  assert.equal(
    canonicalizeRouteHash('#/incidents?incident=i1&slice=1720000000000&q=dns&status=resolved&window=7d').hash,
    '#/incidents?window=7d&status=resolved&q=dns&slice=1720000000000&incident=i1',
  )
  assert.equal(
    canonicalizeRouteHash('#/routes?event=e2&page=3&order=asc&sort=changes&q=x&window=30d&network=blue').hash,
    '#/routes?network=blue&window=30d&q=x&sort=changes&order=asc&page=3&event=e2',
  )
  assert.equal(
    canonicalizeRouteHash('#/targets?page=2&status=incident&kind=external&sort=probes&order=desc').hash,
    '#/targets?kind=external&status=incident&sort=probes&order=desc&page=2',
  )
  assert.equal(
    canonicalizeRouteHash('#/agents?probe=p1&agent=a1&page=4&order=desc&sort=site&health=attention&q=ny').hash,
    '#/agents?q=ny&health=attention&sort=site&order=desc&page=4&agent=a1&probe=p1',
  )
  assert.equal(
    canonicalizeRouteHash('#/pair/ny/lon?metric=loss&window=90d').hash,
    '#/pair/ny/lon?window=90d&metric=loss',
  )
  assert.equal(
    canonicalizeRouteHash('#/target/t1?probe=p1&metric=loss&window=30d').hash,
    '#/target/t1?window=30d&metric=loss&probe=p1',
  )
  assert.equal(canonicalizeRouteHash('#/pair/ny/lon?window=365d').hash, '#/pair/ny/lon?window=365d')
  assert.equal(
    canonicalizeRouteHash('#/settings?probe=p1&site=s1&section=probes').hash,
    '#/settings?section=monitoring&subsection=probes&probe=p1',
  )
  assert.equal(
    canonicalizeRouteHash(
      '#/settings?section=monitoring&subsection=probes&type=dns&enabled=false&mode=direct&page=3&order=desc&sort=updated&q=ny&probe=p1',
    ).hash,
    '#/settings?section=monitoring&subsection=probes&q=ny&page=3&order=desc&sort=updated&mode=direct&enabled=false&type=dns&probe=p1',
  )
  assert.equal(
    canonicalizeRouteHash('#/settings?section=sites&site=site-id&page=2&sort=agents&q=lon').hash,
    '#/settings?section=infrastructure&subsection=sites&q=lon&page=2&sort=agents&site=site-id',
  )
  assert.equal(
    canonicalizeRouteHash('#/settings?section=access&subsection=banner').hash,
    '#/settings?section=access&subsection=users',
  )
})

test('incident route links preserve plane and window and select the event', () => {
  assert.equal(
    routeEventHref('event-2', '30d', '#/incidents?network=blue&window=30d&slice=1720000000000&incident=i1'),
    '#/routes?network=blue&window=30d&event=event-2',
  )
  assert.equal(routeEventHref('event-1', '24h', '#/incidents?incident=i1'), '#/routes?event=event-1')
})

test('incident resource links are scoped and unavailable identities stay plain', () => {
  const source = '#/incidents?network=blue&window=7d&incident=i1'
  assert.equal(
    incidentAgentHref('agent-1', 'probe-1', 'lon-1', source),
    '#/agents?network=blue&agent=agent-1&probe=probe-1',
  )
  assert.equal(incidentAgentHref('agent-1', 'probe-1', '', source), null)
  assert.equal(incidentPairHref('lon', 'nyc', '7d', source), '#/pair/lon/nyc?network=blue&window=7d')
  assert.equal(incidentPairHref('lon', null, '7d', source), null)
  assert.equal(
    incidentTargetHref('target-1', 'probe-1', 'edge', '7d', source),
    '#/target/target-1?network=blue&window=7d&probe=probe-1',
  )
  assert.equal(incidentTargetHref('target-1', 'probe-1', null, '7d', source), null)
})

test('incident expansion follows browser Back and Forward history', () => {
  const locationDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'location')
  const historyDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'history')
  const windowDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'window')
  const browserWindow = new EventTarget()
  const browserLocation = { hash: '#/incidents?window=7d&slice=1720000000000' }
  const entries = [browserLocation.hash]
  let position = 0
  const browserHistory = {
    pushState(_state, _title, hash) {
      entries.splice(position + 1)
      entries.push(hash)
      position++
      browserLocation.hash = hash
    },
    replaceState(_state, _title, hash) {
      entries[position] = hash
      browserLocation.hash = hash
    },
    back() {
      if (position === 0) return
      browserLocation.hash = entries[--position]
      browserWindow.dispatchEvent(new Event('popstate'))
    },
    forward() {
      if (position === entries.length - 1) return
      browserLocation.hash = entries[++position]
      browserWindow.dispatchEvent(new Event('popstate'))
    },
  }
  Object.defineProperty(globalThis, 'location', { configurable: true, value: browserLocation })
  Object.defineProperty(globalThis, 'history', { configurable: true, value: browserHistory })
  Object.defineProperty(globalThis, 'window', { configurable: true, value: browserWindow })
  try {
    const observed = []
    const unsubscribe = subscribeRouteState(() => observed.push(routeParam(browserLocation.hash, 'incident')))
    updateRouteParams({ incident: 'i-copy' })
    assert.equal(browserLocation.hash, '#/incidents?window=7d&slice=1720000000000&incident=i-copy')
    browserHistory.back()
    assert.equal(routeParam(browserLocation.hash, 'incident'), '')
    browserHistory.forward()
    assert.equal(routeParam(browserLocation.hash, 'incident'), 'i-copy')
    assert.deepEqual(observed, ['i-copy', '', 'i-copy'])
    unsubscribe()
  } finally {
    if (locationDescriptor) Object.defineProperty(globalThis, 'location', locationDescriptor)
    else delete globalThis.location
    if (historyDescriptor) Object.defineProperty(globalThis, 'history', historyDescriptor)
    else delete globalThis.history
    if (windowDescriptor) Object.defineProperty(globalThis, 'window', windowDescriptor)
    else delete globalThis.window
  }
})

test('route mutations stop at the installed dirty-form blocker', () => {
  const locationDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'location')
  const historyDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'history')
  const windowDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'window')
  const browserWindow = new EventTarget()
  const browserLocation = { hash: '#/settings?section=monitoring&subsection=thresholds' }
  const browserHistory = {
    pushState(_state, _title, hash) {
      browserLocation.hash = hash
    },
    replaceState(_state, _title, hash) {
      browserLocation.hash = hash
    },
  }
  Object.defineProperty(globalThis, 'location', { configurable: true, value: browserLocation })
  Object.defineProperty(globalThis, 'history', { configurable: true, value: browserHistory })
  Object.defineProperty(globalThis, 'window', { configurable: true, value: browserWindow })
  try {
    let attempted = ''
    setRouteNavigationBlocker((hash) => {
      attempted = hash
      return false
    })
    const result = updateRouteParams({ section: 'appearance', subsection: 'banner' })
    assert.equal(attempted, '#/settings?section=appearance&subsection=banner')
    assert.equal(result, '#/settings?section=monitoring&subsection=thresholds')
    assert.equal(browserLocation.hash, '#/settings?section=monitoring&subsection=thresholds')
  } finally {
    setRouteNavigationBlocker(null)
    if (locationDescriptor) Object.defineProperty(globalThis, 'location', locationDescriptor)
    else delete globalThis.location
    if (historyDescriptor) Object.defineProperty(globalThis, 'history', historyDescriptor)
    else delete globalThis.history
    if (windowDescriptor) Object.defineProperty(globalThis, 'window', windowDescriptor)
    else delete globalThis.window
  }
})

test('dirty settings drafts allow local URL state but guard destructive context changes', () => {
  const site = '#/settings?section=infrastructure&subsection=sites&site=site-1'
  assert.equal(routeChangeDiscardsSettingsDraft(site, site + '&q=lon&page=2'), false)
  assert.equal(routeChangeDiscardsSettingsDraft(site, '#/settings?section=infrastructure&subsection=sites&q=lon'), true)
  assert.equal(
    routeChangeDiscardsSettingsDraft(site, '#/settings?section=monitoring&subsection=targets&site=site-1'),
    true,
  )
  assert.equal(
    routeChangeDiscardsSettingsDraft(
      site,
      '#/settings?network=blue&section=infrastructure&subsection=sites&site=site-1',
    ),
    true,
  )
})

test('typed readers use canonical values and safe integer fallbacks', () => {
  assert.equal(routeParam('#/pair/a/b?metric=loss', 'metric'), 'loss')
  assert.equal(routeParam('#/pair/a/b?metric=bogus', 'metric'), '')
  assert.equal(routeNumberParam('#/routes?page=7', 'page', 1), 7)
  assert.equal(routeNumberParam('#/routes?page=999999999999999999999', 'page', 1), 1)
})

test('target inventory links preserve canonical URL-backed context', () => {
  const inventory = '#/targets?network=blue&q=edge&kind=external&status=incident&sort=probes&order=desc&page=2'
  const detail = targetDetailHref('2f2a264e-0d9f-4fc7-8032-41d00448e278', inventory)
  assert.equal(
    detail,
    '#/target/2f2a264e-0d9f-4fc7-8032-41d00448e278?network=blue&from=%23%2Ftargets%3Fnetwork%3Dblue%26q%3Dedge%26kind%3Dexternal%26status%3Dincident%26sort%3Dprobes%26order%3Ddesc%26page%3D2',
  )
  assert.equal(targetInventoryHref(detail), inventory)
  assert.equal(
    targetInventoryHref('#/target/id?network=green&from=%23%2Ftargets%3Fnetwork%3Dblue%26q%3Dedge%26sort%3Dcreated'),
    '#/targets?network=green&q=edge&sort=created',
  )
  assert.equal(targetInventoryHref('#/target/id?from=%23%2Ftargets%3Fnetwork%3Dblue%26q%3Dedge'), '#/targets?q=edge')
  assert.equal(targetInventoryHref('#/target/id?network=blue&from=%23%2Froutes%3Fq%3Dx'), '#/targets?network=blue')
})
