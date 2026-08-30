import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import test from 'node:test'

// Source-level accessibility gate (issue #102, epic #107). There is no DOM
// in this test environment, so like the rest of the suite these tests read
// component source as text. Runtime behavior (screen reader announcements,
// focus order) is covered by the manual protocol in docs/accessibility.md.
// Contrast is enforced here at the source: the token pairs from the
// measured table in docs/accessibility.md are recomputed from styles.css
// on every run (#106).

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

  for (const file of ['../src/components/ChangePasswordDialog.tsx', '../src/components/UserCreateDialog.tsx']) {
    const source = readFileSync(new URL(file, import.meta.url), 'utf8')
    assert.match(source, /<dialog[^>]*\n?[^>]*aria-labelledby=\{/, `${file} dialog is named by its heading`)
  }
  const mutation = readFileSync(new URL('../src/settingsMutation.tsx', import.meta.url), 'utf8')
  assert.match(mutation, /aria-labelledby=\{confirmTitleID\}/)
})

// #105's keyboard-visualization contract: the slot-grid strips (health
// strip, incident timeline) are roving-tabindex button composites over
// decorative svgs; the chart host carries a keyboard investigation path
// (pure range math from chartRange.ts) plus an sr-only series digest; the
// world map is a labeled image with native keyboard/pointer listeners,
// HTML marker buttons, a visible key hint, and a debounced zoom announcer;
// the sign-ins chart pairs its hover readout with an sr-only breakdown.
test('charts, strips, and the map keep their keyboard access contract', () => {
  for (const file of ['../src/components/HealthStrip.tsx', '../src/components/IncidentTimeline.tsx']) {
    const strip = readFileSync(new URL(file, import.meta.url), 'utf8')
    assert.match(strip, /role="group"/, `${file} wraps its slots in a labelled group`)
    assert.match(strip, /aria-hidden="true"/, `${file} svg is decorative`)
    assert.match(strip, /aria-pressed=/, `${file} slot buttons expose their toggle state`)
    assert.match(strip, /e\.key === 'ArrowLeft'/, `${file} roves focus with the arrow keys`)
    assert.match(strip, /tabIndex=\{i === focusI \? 0 : -1\}/, `${file} keeps one roving tab stop`)
  }

  const chart = readFileSync(new URL('../src/components/Chart.tsx', import.meta.url), 'utf8')
  assert.match(chart, /host\.tabIndex = 0/)
  assert.match(chart, /addEventListener\('keydown', onKeyDown\)/)
  assert.match(chart, /zoomChartRange\(/)
  assert.match(chart, /panChartRange\(/)
  assert.match(chart, /className="sr-only"/)
  assert.match(chart, /role="status"/)

  const map = readFileSync(new URL('../src/components/WorldMap.tsx', import.meta.url), 'utf8')
  assert.doesNotMatch(map, /role="application"/)
  assert.match(map, /role="img"/)
  assert.match(map, /className=\{`map-marker sev-\$\{sev\}`/)
  assert.match(map, /aria-describedby=\{hintId\}/)
  assert.match(map, /setTimeout\(\(\) => setAnnouncedZoom\(zoomPercent\), 500\)/)

  const users = readFileSync(new URL('../src/components/LoginBars.tsx', import.meta.url), 'utf8')
  assert.match(users, /Sign-ins by month: /)
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

// ---- #106: measured contrast, type scaling, forced colors ----

const stylesSource = readFileSync(new URL('../src/styles.css', import.meta.url), 'utf8')

// The two top-level token blocks (light `:root`, dark override). The ^
// anchor keeps the `:root` nested inside @media (prefers-contrast) from
// matching.
function parseTokenBlock(body) {
  return Object.fromEntries(
    [...body.replaceAll(/\/\*[\s\S]*?\*\//g, '').matchAll(/--([a-z0-9-]+):\s*([^;]+);/g)].map((m) => [
      m[1],
      m[2].trim(),
    ]),
  )
}

function themeTokens() {
  const blocks = [...stylesSource.matchAll(/^:root(\[data-theme='dark'\])?\s*\{([^}]*)\}/gm)]
  assert.equal(blocks.length, 2, 'styles.css holds exactly one light and one dark token block')
  const light = parseTokenBlock(blocks[0][2])
  // Dark declares only overrides; everything else inherits from light.
  return { light, dark: { ...light, ...parseTokenBlock(blocks[1][2]) } }
}

// Resolves a token to sRGB channels ([r, g, b] in 0-255, kept as floats),
// following var() chains and the color-mix(in srgb, <hex> <pct>%,
// var(--token)) shape the status backgrounds use. Mixing stays in
// floating point — CSS color-mix does not round to 8-bit — so the
// asserted ratios are exactly what ships: two pairs (light crit on
// surface, dark ok on ok-bg) clear 4.5:1 by under 0.05, and any change
// to a surface or a mix percentage must re-clear them.
function resolveColor(tokens, name) {
  let value = tokens[name]
  assert.ok(value, `token --${name} exists`)
  for (let hops = 0; value.startsWith('var('); hops++) {
    assert.ok(hops < 4, `--${name} var() chain terminates`)
    value = tokens[value.slice('var(--'.length, -1)]
  }
  const mix = value.match(/^color-mix\(in srgb, (#[0-9a-f]{6}) (\d+)%, var\(--([a-z0-9-]+)\)\)$/)
  if (mix) {
    const pct = Number(mix[2]) / 100
    const base = channels(mix[1])
    const rest = resolveColor(tokens, mix[3])
    return base.map((v, i) => v * pct + rest[i] * (1 - pct))
  }
  assert.match(value, /^#[0-9a-f]{6}$/, `--${name} resolves to a 6-digit hex color, got: ${value}`)
  return channels(value)
}

function channels(hex) {
  return [1, 3, 5].map((i) => Number.parseInt(hex.slice(i, i + 2), 16))
}

// WCAG 2.x relative luminance and contrast ratio over channel triples.
function luminance(rgb) {
  const [r, g, b] = rgb.map((v) => {
    const c = v / 255
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function contrast(fg, bg) {
  const [hi, lo] = [luminance(fg), luminance(bg)].toSorted((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}

// The measured table from docs/accessibility.md: [fg, bg, threshold].
// 4.5:1 pairs are text roles (WCAG 1.4.3); 3:1 pairs are non-text marks
// (1.4.11). Deliberate exemptions (map dot grid, series adjacency, canvas
// axis text) are documented there, not asserted here.
const CONTRAST_PAIRS = [
  ['ink', 'bg', 4.5],
  ['ink', 'surface', 4.5],
  ['ink', 'surface-2', 4.5],
  ['ink-2', 'bg', 4.5],
  ['ink-2', 'surface', 4.5],
  ['ink-2', 'surface-2', 4.5],
  ['ink-3', 'bg', 4.5],
  ['ink-3', 'surface', 4.5],
  ['ink-3', 'surface-2', 4.5],
  ['accent-ink', 'accent', 4.5],
  ['accent', 'surface', 4.5],
  ['accent', 'bg', 4.5],
  ['status-ok', 'surface', 4.5],
  ['status-ok', 'status-ok-bg', 4.5],
  ['status-degraded', 'surface', 4.5],
  ['status-degraded', 'status-degraded-bg', 4.5],
  ['status-down', 'surface', 4.5],
  ['status-down', 'status-down-bg', 4.5],
  ['status-stale', 'surface', 4.5],
  ['status-stale', 'status-stale-bg', 4.5],
  ['status-warn', 'surface', 4.5],
  ['status-crit', 'surface', 4.5],
  ['focus', 'bg', 3],
  ['focus', 'surface', 3],
  ['series-a', 'surface', 3],
  ['series-b', 'surface', 3],
  ['chart-local', 'surface', 3],
  ['chart-oidc', 'surface', 3],
  // Hovered fleet rows and inset panels paint surface-2 behind these
  // marks, so they must clear the bar on both grounds.
  ['strip-nodata', 'surface', 3],
  ['strip-nodata', 'surface-2', 3],
  ['control-border', 'surface', 3],
  ['control-border', 'surface-2', 3],
]

test('every token pair in the measured contrast table meets its bar in both themes', () => {
  const themes = themeTokens()
  for (const [themeName, tokens] of Object.entries(themes)) {
    for (const [fg, bg, threshold] of CONTRAST_PAIRS) {
      const ratio = contrast(resolveColor(tokens, fg), resolveColor(tokens, bg))
      assert.ok(
        ratio >= threshold,
        `${themeName}: --${fg} on --${bg} is ${ratio.toFixed(2)}:1, below the ${threshold}:1 bar`,
      )
    }
  }
})

// WCAG 1.4.4: the root type size must follow the browser's font-size
// preference, so no pixel root; visible text keeps the project's 0.75rem
// floor (#76 set it for chart labels, #106 extends it dashboard-wide).
// The only px type is PathGraph's two 12px SVG labels — fixed user units
// that scale with the viewBox (see PathGraph.tsx), pinned by
// topology.test.mjs.
test('type scales from a rem root and visible text stays at or above 0.75rem', () => {
  assert.match(stylesSource, /body\s*\{[^}]*font-size:\s*0\.9375rem/)
  assert.doesNotMatch(stylesSource, /body\s*\{[^}]*font-size:[^}]*px/)

  const pxSizes = [...stylesSource.matchAll(/font-size:\s*([\d.]+)px/g)]
  assert.equal(pxSizes.length, 2, 'only the two PathGraph SVG label rules may use px type')
  for (const match of pxSizes) assert.equal(match[1], '12')

  for (const match of stylesSource.matchAll(/font-size:\s*([\d.]+)(rem|em)/g)) {
    assert.ok(Number(match[1]) >= 0.75, `font-size ${match[0].trim()} sits below the 0.75rem floor`)
  }
})

// Forced colors (Windows High Contrast) and increased-contrast support:
// focus rides the system Highlight, data marks that carry severity or
// series identity keep their AA-verified palette via forced-color-adjust
// opt-outs, and prefers-contrast: more lifts the lowest-margin roles.
test('forced-colors and prefers-contrast handling stays in place', () => {
  const forcedBlock = stylesSource.match(/@media \(forced-colors: active\) \{[\s\S]*?\n\}/)?.[0]
  assert.ok(forcedBlock, 'styles.css carries a forced-colors block')
  assert.match(forcedBlock, /:focus-visible\s*\{[^}]*outline-color:\s*Highlight/)
  assert.match(forcedBlock, /\.search-field input:focus\s*\{[^}]*Highlight/)
  for (const island of ['.swatch', '.fleet-strip', '.login-bars', '.itl-wrap', '.worldmap', '.chart-card']) {
    assert.ok(
      new RegExp(`${island.replaceAll('.', '\\.')}[^}]*\\{[^}]*forced-color-adjust:\\s*none`).test(forcedBlock) ||
        new RegExp(`${island.replaceAll('.', '\\.')},`).test(forcedBlock),
      `${island} keeps its verified palette under forced colors`,
    )
  }
  // An island without its own ground would strand its kept palette on a
  // forced Canvas of the opposite luminance (dark theme, light system
  // palette), so every opt-out rule must pin the themed surface.
  for (const rule of forcedBlock.matchAll(/\{([^}]*forced-color-adjust:\s*none[^}]*)\}/g)) {
    assert.match(rule[1], /background:\s*var\(--surface\)/, 'forced-color islands carry their themed backdrop')
  }
  assert.match(forcedBlock, /\.swatch\s*\{[^}]*border:\s*1px solid CanvasText/)

  const contrastBlock = stylesSource.match(/@media \(prefers-contrast: more\) \{[\s\S]*?\n\}/)?.[0]
  assert.ok(contrastBlock, 'styles.css carries a prefers-contrast block')
  assert.match(contrastBlock, /--ink-3:\s*var\(--ink-2\)/)
  // :root[data-theme='dark'] outranks a bare :root, so the override must
  // restate the dark selector or dark users get no increased contrast.
  assert.match(contrastBlock, /:root\[data-theme='dark'\]/)
})
