import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  dataTableMissingKeys,
  dataTablePageCount,
  dataTablePageNumber,
  dataTableResultRange,
  nextDataTableSort,
} from '../src/dataTableState.ts'

const component = readFileSync(new URL('../src/components/DataTable.tsx', import.meta.url), 'utf8')
const styles = readFileSync(new URL('../src/styles.css', import.meta.url), 'utf8')

test('controlled sorting and paging have stable transitions and metadata', () => {
  assert.deepEqual(nextDataTableSort({ key: 'name', order: 'asc' }, 'name'), { key: 'name', order: 'desc' })
  assert.deepEqual(nextDataTableSort({ key: 'name', order: 'desc' }, 'site'), { key: 'site', order: 'asc' })

  const page = { limit: 25, offset: 25, total: 63, has_more: true }
  assert.equal(dataTablePageNumber(page), 2)
  assert.equal(dataTablePageCount(page), 3)
  assert.equal(dataTableResultRange(page, 25), 'Showing 26–50 of 63')
  assert.equal(dataTableResultRange({ ...page, offset: 75, has_more: false }, 0), '0 results')
})

test('refresh reconciliation clears missing disclosure and action identities', () => {
  assert.deepEqual(dataTableMissingKeys(['agent-1', 'agent-3'], 'agent-2', 'agent-3'), {
    expandedMissing: true,
    actionMissing: false,
  })
  assert.deepEqual(dataTableMissingKeys(['agent-1'], null, 'agent-2'), {
    expandedMissing: false,
    actionMissing: true,
  })
  assert.match(component, /focusedRow\.current !== null && !keys\.includes\(focusedRow\.current\)/)
  assert.match(component, /root\.current\?\.focus\(\)/)
  assert.match(component, /role="region"/)
  assert.match(component, /aria-label=\{`\$\{label\} data table`\}/)
})

test('component exposes sortable announcements, paging, disclosures, menus, and all states', () => {
  assert.match(component, /aria-sort=/)
  assert.match(component, /aria-live="polite"/)
  assert.match(component, />\s*Previous\s*</)
  assert.match(component, />\s*Next\s*</)
  assert.match(component, /className="data-table-disclosure"/)
  assert.match(component, /className="data-table-actions-menu"/)
  assert.match(component, /createPortal/)
  assert.match(component, /event\.key !== 'Escape'/)
  assert.match(component, /loading && rows\.length === 0/)
  assert.match(component, /error && rows\.length === 0/)
  assert.match(component, /data-table-state empty-state/)
})

test('floating actions escape the scroll region and disclosure identities are surface-qualified', () => {
  assert.match(styles, /\.data-table-actions-menu\s*\{[^}]*position:\s*fixed/)
  assert.doesNotMatch(styles, /\.data-table-actions-menu\s*\{[^}]*position:\s*absolute/)
  assert.match(component, /render\(row, 'desktop'\)/)
  assert.match(component, /render\(row, 'mobile'\)/)

  const agents = readFileSync(new URL('../src/views/Agents.tsx', import.meta.url), 'utf8')
  assert.match(agents, /agent-probe-\$\{selectedProbe\}-\$\{surface\}/)
  assert.match(agents, /render: \(row, surface\)/)
})

test('mobile list hides secondary metadata behind a full-width touch target', () => {
  assert.match(component, /priority !== 'secondary'/)
  assert.match(component, /priority === 'secondary'/)
  assert.match(styles, /\.data-table-mobile\s*\{\s*display:\s*none/)
  assert.match(styles, /@media \(max-width: 760px\)[\s\S]*?\.data-table-desktop\s*\{\s*display:\s*none/)
  assert.match(
    styles,
    /\.data-table-mobile-row > \.data-table-disclosure[\s\S]*?width:\s*100%[\s\S]*?min-height:\s*44px/,
  )
  assert.doesNotMatch(styles, /\.data-table-mobile[^{}]*\{[^}]*overflow-x:\s*auto/)
})

test('all adopted inventories request server pages and render the shared component', () => {
  const files = [
    '../src/views/Agents.tsx',
    '../src/views/Paths.tsx',
    '../src/views/Targets.tsx',
    '../src/components/SitesPanel.tsx',
    '../src/components/TargetsPanel.tsx',
    '../src/components/ProbesPanel.tsx',
  ]
  for (const file of files) {
    const source = readFileSync(new URL(file, import.meta.url), 'utf8')
    assert.match(source, /<DataTable/)
    assert.match(source, /limit:\s*String\([^)]*PAGE\)/)
    assert.match(source, /\(page - 1\) \* [A-Z_]*PAGE/)
    assert.doesNotMatch(source, /\.slice\(\(page - 1\)/)
  }
})

test('debounced route values drive inventory requests and linked rows keep scope context', () => {
  for (const file of ['../src/views/Agents.tsx', '../src/views/Paths.tsx']) {
    const source = readFileSync(new URL(file, import.meta.url), 'utf8')
    assert.match(source, /const \[queryParam\] = useRouteParam\('q'\)/)
    assert.match(source, /queryParam\.trim\(\)/)
    assert.doesNotMatch(source, /else if \(query\.trim\(\)\) params\.set\('q'/)
    assert.match(source, /needsScopeRequest/)
    assert.match(source, /data-table-context/)
  }
})
