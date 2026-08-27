import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import test from 'node:test'

// Source-level accessibility gate (issue #102, epic #107). There is no DOM
// in this test environment, so like the rest of the suite these tests read
// component source as text. Runtime behavior (screen reader announcements,
// focus order, contrast) is covered by the manual protocol in
// docs/accessibility.md. Contrast-token assertions join this file once the
// measured table from issue #106 lands.

const srcRoot = new URL('../src/', import.meta.url).pathname

function sourceFiles(extension) {
  return readdirSync(srcRoot, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(extension))
    .map((entry) => {
      const path = join(entry.parentPath, entry.name)
      return { path: relative(srcRoot, path).split(sep).join('/'), source: readFileSync(path, 'utf8') }
    })
}

// Heading levels are checked per file in source order: at most one <h1>, and
// no heading may sit more than one level below its predecessor. Files are
// composed (settings panels legitimately open at <h2> under their view's
// <h1>), so the first heading in a file sets that file's base level.
function headingViolations(source) {
  const violations = []
  let previous = null
  let h1Count = 0
  for (const match of source.matchAll(/<h([1-6])[\s>]/g)) {
    const level = Number(match[1])
    if (level === 1 && ++h1Count > 1) violations.push('more than one <h1>')
    if (previous !== null && level > previous + 1) violations.push(`skips <h${previous}> to <h${level}>`)
    previous = level
  }
  return violations
}

// Emptied by #103. A file gaining a heading defect must be fixed, not
// baselined; entries here pin a defect's exact violations only while a
// tracked issue owns the fix.
const KNOWN_HEADING_VIOLATIONS = {}

test('heading levels descend without skips in every component', () => {
  for (const { path, source } of sourceFiles('.tsx')) {
    const violations = headingViolations(source)
    assert.deepEqual(
      violations,
      KNOWN_HEADING_VIOLATIONS[path] ?? [],
      `${path} heading-order violations diverge from the #103 baseline`,
    )
  }
})

// The full jsx-a11y rule set runs at error severity (.oxlintrc.json), so a
// suppression comment is the only way a violation can land. Directives are
// inventoried rule by rule (not per comment), so widening an existing
// comment with one more rule fails here just like a new comment; removing
// one (#104, #105) must shrink the baseline.
function disableDirectives(source) {
  // Everything after ` -- ` is explanation, not rule selection.
  return [...source.matchAll(/oxlint-disable[a-z-]*([^\n]*)/g)].map((match) => match[1].split(' -- ')[0])
}

test('jsx-a11y lint suppressions match the known baseline rule for rule', () => {
  const suppressed = {}
  for (const extension of ['.tsx', '.ts']) {
    for (const { path, source } of sourceFiles(extension)) {
      const rules = disableDirectives(source)
        .flatMap((directive) => directive.match(/jsx-a11y\/[a-z-]+/g) ?? [])
        .toSorted()
      if (rules.length > 0) suppressed[path] = rules
    }
  }
  assert.deepEqual(suppressed, {
    // Deliberate: onFocusCapture bookkeeping on the table region so a
    // refresh that removes the focused row can restore focus — not an
    // interaction the rule should see.
    'components/DataTable.tsx': [
      'jsx-a11y/no-noninteractive-element-interactions',
      'jsx-a11y/no-static-element-interactions',
    ],
    // Pointer-only slot drill-down, fix tracked by #105.
    'components/HealthStrip.tsx': [
      'jsx-a11y/click-events-have-key-events',
      'jsx-a11y/no-noninteractive-element-interactions',
    ],
    // Pointer-only chart interaction, fix tracked by #105.
    'components/IncidentTimeline.tsx': [
      'jsx-a11y/click-events-have-key-events',
      'jsx-a11y/no-noninteractive-element-interactions',
    ],
    // Pointer-only hover readout on the sign-ins chart svg (same class of
    // gap as HealthStrip), fix tracked by #105.
    'components/UsersPanel.tsx': ['jsx-a11y/no-noninteractive-element-interactions'],
    // Focusable map svg + tip keep-open handlers, fix tracked by #105.
    'components/WorldMap.tsx': [
      'jsx-a11y/no-noninteractive-element-interactions',
      'jsx-a11y/no-noninteractive-element-interactions',
      'jsx-a11y/no-noninteractive-tabindex',
    ],
    // Deliberate: autofocus on the single-purpose login form.
    'views/Login.tsx': ['jsx-a11y/no-autofocus'],
  })
})

// A rule-less oxlint-disable directive suppresses every rule on its target,
// silently bypassing both the lint gate and the inventory above.
test('every lint suppression names the rules it disables', () => {
  for (const extension of ['.tsx', '.ts']) {
    for (const { path, source } of sourceFiles(extension)) {
      for (const directive of disableDirectives(source)) {
        assert.match(directive, /[a-z0-9-]+\/[a-z0-9-]+/, `${path} has a blanket oxlint-disable directive`)
      }
    }
  }
})

// Files that render inline validation lists must use the shared summary
// hook (aria-describedby + focus-on-request; formErrors.ts). This is a
// presence-level pin — per-list wiring depth is covered by the manual
// protocol in docs/accessibility.md, not by source regexes.
test('validation error lists are wired through useErrorSummary', () => {
  for (const { path, source } of sourceFiles('.tsx')) {
    if (!source.includes('className="error threshold-errors"')) continue
    assert.match(source, /useErrorSummary\(/, `${path} renders a validation error list without useErrorSummary`)
  }
  const login = readFileSync(new URL('../src/views/Login.tsx', import.meta.url), 'utf8')
  const invalidCount = [...login.matchAll(/aria-invalid=\{credentialError\}/g)].length
  assert.equal(invalidCount, 2, 'both Login fields carry credential-gated aria-invalid wiring')
})

// #104's composite-widget contract: <details> menus route through
// DisclosureMenu (Escape / roving / dismissal), exclusive switchers are
// radiogroups, the portal'd row-action popup is a labelled group with
// keyboard dismissal, the drawer inerts the backgrounded app, and native
// dialogs are named by their rendered heading.
test('composite widgets keep their menu, group, and dialog semantics', () => {
  const app = readFileSync(new URL('../src/App.tsx', import.meta.url), 'utf8')
  assert.match(app, /<DisclosureMenu/)
  assert.doesNotMatch(app, /<details/)

  const filter = readFileSync(new URL('../src/components/TopbarFilter.tsx', import.meta.url), 'utf8')
  assert.match(filter, /<DisclosureMenu/)
  assert.match(filter, /<RadioButtonGroup/)

  const connectivity = readFileSync(new URL('../src/components/ConnectivityCard.tsx', import.meta.url), 'utf8')
  assert.match(connectivity, /<RadioButtonGroup/)

  const radioGroup = readFileSync(new URL('../src/components/RadioButtonGroup.tsx', import.meta.url), 'utf8')
  assert.match(radioGroup, /role="radiogroup"/)
  assert.match(radioGroup, /role="radio"/)

  const table = readFileSync(new URL('../src/components/DataTable.tsx', import.meta.url), 'utf8')
  assert.doesNotMatch(table, /role="toolbar"/)
  assert.match(table, /event\.key === 'Tab'/)
  assert.match(table, /closest\('dialog'\)/)

  const drawer = readFileSync(new URL('../src/components/MobileNavigation.tsx', import.meta.url), 'utf8')
  assert.match(drawer, /setAttribute\('inert', ''\)/)
  assert.match(drawer, /removeAttribute\('inert'\)/)

  for (const file of ['../src/components/ChangePasswordDialog.tsx', '../src/components/UsersPanel.tsx']) {
    const source = readFileSync(new URL(file, import.meta.url), 'utf8')
    assert.match(source, /<dialog[^>]*\n?[^>]*aria-labelledby=\{/, `${file} dialog is named by its heading`)
  }
  const mutation = readFileSync(new URL('../src/settingsMutation.tsx', import.meta.url), 'utf8')
  assert.match(mutation, /aria-labelledby=\{confirmTitleID\}/)
})

test('global accessibility scaffolding stays in place', () => {
  const app = readFileSync(new URL('../src/App.tsx', import.meta.url), 'utf8')
  assert.match(app, /className="skip-link"/)
  assert.match(app, /<main id="main-content" tabIndex=\{-1\}>/)

  const styles = readFileSync(new URL('../src/styles.css', import.meta.url), 'utf8')
  assert.match(styles, /\.sr-only\s*\{/)
  assert.match(styles, /:focus-visible\s*\{[^}]*outline/)
  assert.match(styles, /@media \(prefers-reduced-motion/)

  const index = readFileSync(new URL('../index.html', import.meta.url), 'utf8')
  assert.match(index, /<html lang="en">/)
  assert.match(index, /<meta name="viewport"/)
  // Blocking pinch zoom is a WCAG 1.4.4 failure; neither directive may appear.
  assert.doesNotMatch(index, /maximum-scale|user-scalable/)
})
