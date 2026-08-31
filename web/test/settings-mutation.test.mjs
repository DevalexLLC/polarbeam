import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import { SETTINGS_GROUPS, SETTINGS_TABS } from '../src/settingsTabs.ts'
import { canonicalSnapshot, serverSnapshotChanged, synchronizeDraftBaseline } from '../src/settingsSnapshot.ts'
import { readStyles } from './util/styles.mjs'

test('settings navigation has the required groups and one canonical subsection per tab', () => {
  assert.deepEqual(
    SETTINGS_GROUPS.map((group) => [group.group, group.label]),
    [
      ['monitoring', 'Monitoring'],
      ['infrastructure', 'Infrastructure'],
      ['access', 'Access'],
      ['appearance', 'Appearance'],
    ],
  )
  const expected = {
    thresholds: 'monitoring',
    targets: 'monitoring',
    meshes: 'monitoring',
    probes: 'monitoring',
    sites: 'infrastructure',
    networks: 'infrastructure',
    enrollment: 'infrastructure',
    users: 'access',
    authentication: 'access',
    banner: 'appearance',
  }
  assert.equal(SETTINGS_TABS.length, Object.keys(expected).length)
  for (const tab of SETTINGS_TABS) {
    assert.equal(tab.group, expected[tab.tab])
    assert.equal(tab.href, `#/settings?section=${tab.group}&subsection=${tab.tab}`)
  }
})

test('canonical snapshots ignore object key order but detect server edits', () => {
  const loaded = { enabled: true, nested: { issuer: 'https://idp', scopes: ['openid', 'profile'] } }
  const reordered = { nested: { scopes: ['openid', 'profile'], issuer: 'https://idp' }, enabled: true }
  const changed = { nested: { scopes: ['openid'], issuer: 'https://idp' }, enabled: true }
  assert.equal(canonicalSnapshot(loaded), canonicalSnapshot(reordered))
  assert.equal(serverSnapshotChanged(loaded, reordered), false)
  assert.equal(serverSnapshotChanged(loaded, changed), true)
})

test('opening an editor synchronizes its baseline before computing dirtiness', () => {
  const previous = { id: 'none', networks: [] }
  const loaded = { id: 'user-1', networks: ['blue'] }
  assert.equal(synchronizeDraftBaseline(previous, loaded, true, false), loaded)
  const polled = { id: 'user-1', networks: ['green'] }
  assert.equal(synchronizeDraftBaseline(loaded, polled, true, true), loaded)
  assert.equal(synchronizeDraftBaseline(loaded, polled, false, true), polled)
})

test('settings mutation boundary guards navigation and standardizes feedback', async () => {
  const source = await readFile(new URL('../src/settingsMutation.tsx', import.meta.url), 'utf8')
  assert.match(source, /addEventListener\('beforeunload'/)
  assert.match(source, /setRouteNavigationBlocker\(blockRoute\)/)
  assert.match(source, /cancelLabel: 'Stay'/)
  assert.match(source, /confirmLabel: 'Discard'/)
  assert.match(source, /const SUCCESS_MS = 4_000/)
  assert.match(source, /kind: 'error'/)
  assert.match(source, /Reload server version/)
  assert.match(source, /if \(!serverSnapshotChanged\([\s\S]*return true/)
  assert.match(source, /Your changes were not saved/)
  assert.match(source, /const canonical = canonicalizeRouteHash\(destination\.hash\)\.hash/)
  assert.match(source, /if \(!dialog\.open\) dialog\.showModal\(\)/)
  assert.match(source, /routeChangeDiscardsSettingsDraft/)
  assert.match(source, /pendingRouteRef\.current \|\| confirmationRef\.current/)
  assert.match(source, /if \(!blockRoute\(target\.hash, 'push'\)\) event\.preventDefault\(\)/)
})

test('destructive settings actions name the resource and consequence in one dialog', async () => {
  const button = await readFile(new URL('../src/components/ConfirmButton.tsx', import.meta.url), 'utf8')
  const provider = await readFile(new URL('../src/settingsMutation.tsx', import.meta.url), 'utf8')
  const styles = readStyles()
  assert.match(button, /resource: string/)
  assert.match(button, /consequence: string/)
  assert.doesNotMatch(button, /armed|ARM_MS/)
  assert.match(provider, /cancelRef\.current\?\.focus\(\)/)
  assert.match(provider, /<strong>\{confirmation\.resource\}<\/strong>/)
  assert.match(provider, /\{confirmation\.consequence\}/)
  assert.match(styles, /button\.danger-button/)
  assert.doesNotMatch(styles, /inline-confirm\.armed/)
})

test('every editable settings panel participates in the shared mutation boundary', async () => {
  const files = [
    'BannerSettingsPanel.tsx',
    'OIDCSettingsPanel.tsx',
    'ThresholdSettings.tsx',
    'ThresholdOverrideForm.tsx',
    'NetworksPanel.tsx',
    'SitesPanel.tsx',
    'TargetsPanel.tsx',
    'ProbesPanel.tsx',
    'MeshesPanel.tsx',
    'EnrollmentPanel.tsx',
    'UsersPanel.tsx',
  ]
  for (const file of files) {
    const source = await readFile(new URL(`../src/components/${file}`, import.meta.url), 'utf8')
    assert.match(source, /useSettings(?:Mutation|Draft)|useConcurrentSettingsDraft/, file)
  }
})

test('full-state editors block their mutation when the preflight snapshot changed', async () => {
  const files = [
    'BannerSettingsPanel.tsx',
    'OIDCSettingsPanel.tsx',
    'ThresholdSettings.tsx',
    'ThresholdOverrideForm.tsx',
    'NetworksPanel.tsx',
    'SitesPanel.tsx',
    'TargetsPanel.tsx',
    'ProbesPanel.tsx',
    'UsersPanel.tsx',
  ]
  for (const file of files) {
    const source = await readFile(new URL(`../src/components/${file}`, import.meta.url), 'utf8')
    assert.match(source, /const currentServer = await [\w.]+\.checkForConflict/, file)
    assert.match(source, /if \(!currentServer\) return/, file)
  }
})

test('probe state toggles preflight the same full state they write', async () => {
  const source = await readFile(new URL('../src/components/ProbeRowActions.tsx', import.meta.url), 'utf8')
  const toggle = source.slice(source.indexOf('const setEnabled'), source.indexOf('const remove'))
  assert.match(toggle, /await apiGet<ProbeConfig>/)
  assert.match(toggle, /serverSnapshotChanged/)
  assert.match(toggle, /feedback\.conflict/)
  assert.ok(toggle.indexOf('await apiGet<ProbeConfig>') < toggle.indexOf('await apiPut'))
})

test('mesh create leaves duplicate-name classification to the server', async () => {
  const source = await readFile(new URL('../src/components/MeshesPanel.tsx', import.meta.url), 'utf8')
  const create = source.slice(source.indexOf('<h3 className="eyebrow">Create mesh</h3>'))
  assert.doesNotMatch(create, /checkForConflict/)
  assert.match(create, /apiPost\('\/api\/v1\/config\/meshes'/)
})

test('one-time secrets name their irreversible discard consequence', async () => {
  const enrollment = await readFile(new URL('../src/components/EnrollmentPanel.tsx', import.meta.url), 'utf8')
  const users = await readFile(new URL('../src/components/UserCreateDialog.tsx', import.meta.url), 'utf8')
  assert.match(enrollment, /shown only once and cannot be recovered/)
  assert.match(enrollment, /issuing a replacement token/)
  assert.match(users, /shown only once and cannot be recovered/)
  assert.match(users, /resetting the password again/)
})

test('create collisions preserve target input and destructive labels use verbs', async () => {
  const targets = await readFile(new URL('../src/components/TargetsPanel.tsx', import.meta.url), 'utf8')
  const meshes = await readFile(new URL('../src/components/MeshesPanel.tsx', import.meta.url), 'utf8')
  assert.match(targets, /a target named \$\{draft\.name\.trim\(\)\} already exists/)
  assert.match(targets, /choose another name or edit that target/)
  assert.match(meshes, /label="Remove"/)
  assert.doesNotMatch(meshes, /label="×"/)
})

test('session changes clear notifications and banner layout offsets the sticky sidebar', async () => {
  const app = await readFile(new URL('../src/App.tsx', import.meta.url), 'utf8')
  const styles = readStyles()
  assert.match(app, /useEffect\(\(\) => clearNotifications\(\), \[clearNotifications, user\]\)/)
  assert.match(app, /guardAction\(\(\) => void logout\(\)\)/)
  assert.match(styles, /\.banner-frame \.settings-sidebar[\s\S]*top: calc\(5rem \+ var\(--banner-h\)\)/)
})
