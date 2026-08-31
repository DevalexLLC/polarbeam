import assert from 'node:assert/strict'
import test from 'node:test'
import { PRIMARY_NAVIGATION, SETTINGS_NAVIGATION, navigationItemIsCurrent, wrappedTabIndex } from '../src/navigation.ts'
import { readStyles } from './util/styles.mjs'

test('mobile navigation exposes every primary route in operational order', () => {
  assert.deepEqual(
    [...PRIMARY_NAVIGATION, SETTINGS_NAVIGATION].map(({ href, label }) => [href, label]),
    [
      ['#/', 'Overview'],
      ['#/incidents', 'Incidents'],
      ['#/routes', 'Routes'],
      ['#/targets', 'Targets'],
      ['#/agents', 'Agents'],
      ['#/settings', 'Settings'],
    ],
  )
  assert.equal(navigationItemIsCurrent('#/', 'pair'), true)
  assert.equal(navigationItemIsCurrent('#/targets', 'target'), true)
  assert.equal(navigationItemIsCurrent('#/settings', 'settings'), true)
  assert.equal(navigationItemIsCurrent('#/routes', 'incidents'), false)
})

test('drawer focus wraps in both directions and nowhere else', () => {
  assert.equal(wrappedTabIndex(0, 7, true), 6)
  assert.equal(wrappedTabIndex(6, 7, false), 0)
  assert.equal(wrappedTabIndex(-1, 7, true), 6)
  assert.equal(wrappedTabIndex(-1, 7, false), 0)
  assert.equal(wrappedTabIndex(3, 7, false), null)
  assert.equal(wrappedTabIndex(0, 0, false), null)
})

test('touch-target contract covers navigation, controls, disclosures, and row actions', () => {
  const styles = readStyles()
  const required = [
    '.topnav a',
    '.mobile-nav-toggle',
    '.mobile-nav-link',
    '.control-group button',
    '.incident-summary',
    'button.agent-detail-toggle',
    'button.path-toggle',
    'td.config-actions .secondary-button',
    'td.users-actions .inline-confirm',
  ]
  const rules = [...styles.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((match) => ({
    selectors: match[1]
      .replaceAll(/\/\*[\s\S]*?\*\//g, '')
      .split(',')
      .map((selector) => selector.trim()),
    body: match[2],
  }))

  for (const selector of required) {
    const rule = rules.find(
      (candidate) =>
        candidate.selectors.includes(selector) &&
        /min-height:\s*(?:var\(--control-target\)|max\(72px, var\(--control-target\)\))/.test(candidate.body) &&
        /min-width:\s*var\(--minimum-target\)/.test(candidate.body),
    )
    assert.ok(rule, `missing touch-target rule for ${selector}`)
  }
  assert.match(styles, /--control-target:\s*40px/)
  assert.match(styles, /\(pointer:\s*coarse\)[^{]*\{[\s\S]*?--control-target:\s*44px/)
})
