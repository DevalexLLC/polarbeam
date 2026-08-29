# Accessibility

## Conformance target

The PolarBEAM dashboard targets **WCAG 2.1 Level AA**, which satisfies
Section 508 (whose baseline is WCAG 2.0 Level A/AA) and adds the reflow,
non-text-contrast, and pointer-gesture criteria that matter for the map,
charts, and status indicators. The work is tracked by epic #107; this
document describes the gates that hold the line while the remaining
children land and after they are done.

## Automated gate

Two mechanisms run in the `web-lint` CI job and in `make web`:

- **Lint.** Every oxlint `jsx-a11y` rule runs at error severity
  (`web/.oxlintrc.json`), with one deliberate exception described below.
  `pnpm run lint` uses `--deny-warnings`, so nothing accessibility-shaped
  can land as a warning.
- **Source-level tests.** `web/test/a11y-source.test.mjs` checks what the
  linter cannot: per-file heading-level order, a rule-by-rule inventory of
  `jsx-a11y` lint suppressions (blanket rule-less directives are rejected
  outright), and the presence of global scaffolding
  (skip link, focusable `main`, `.sr-only`, `:focus-visible` outline,
  reduced-motion blocks, `lang`, and an unblocked viewport). The two
  baselines in that file — heading violations and suppressions — may only
  shrink: fixing a listed defect requires deleting its baseline entry, and
  adding a new defect fails the test. Since #106 the file also recomputes
  the [contrast token table](#contrast-tokens) from `styles.css` on every
  run — parsing both theme blocks, resolving `var()` and `color-mix`
  exactly as browsers do, and failing any pair that drops below its bar —
  and pins the rem type root, the 0.75rem visible-type floor, and the
  forced-colors / prefers-contrast blocks.

There is deliberately no browser-based accessibility job: the SPA test
environment is DOM-free `node --test`, and runtime verification is manual.

## Manual test protocol

Run before merging changes that touch views, shared components, or
`styles.css`, at both reference viewports (1440x1000 and 390x844) in both
themes:

1. **Keyboard-only walkthrough.** Unplug the mouse figuratively: Tab/Shift+
   Tab through the changed views. Every visible control must be reachable,
   operable (Enter/Space, arrows where a widget defines them, Escape to
   dismiss), and show the focus outline. Dialogs must trap focus and return
   it to their trigger on close. The composite visualizations have their
   own keyboard contracts: health-strip and incident-timeline slots are one
   roving tab stop (arrows/Home/End move, Enter/Space pins or filters),
   charts pan with arrows and zoom with +/− (Escape/0 returns live), and
   the map pans/zooms from its focused surface (arrows, +/−, F, 0) with
   each site marker a separate button.
2. **Assistive-technology scan.** Run ANDI (section508.gov's bookmarklet)
   or the axe DevTools browser extension against the changed views; no new
   WCAG 2.1 AA violations.
3. **Contrast spot checks.** Pairs built from tokens are enforced by the
   automated table below; spot-check only *novel* pairs — a hardcoded
   color, a new color-mix, an image overlay — at 4.5:1 for text and 3:1
   for non-text indicators in both themes (WebAIM contrast checker or
   equivalent), and add any new token pair to the table.
4. **Text scaling and reflow.** At 200% browser text size (font-size
   preference, not zoom) the whole type ladder scales together and the
   changed views lose nothing. At 400% page zoom on a 1280px window —
   equivalently a 320 CSS px viewport — content reflows into the narrow
   layout with no horizontal scrolling of body content; only data tables
   (the matrix's `.scroll-x` wrapper) may pan sideways within their own
   scroll container.
5. **Forced colors.** With forced colors emulated (DevTools → Rendering →
   `forced-colors: active`, or Windows High Contrast) in both themes:
   focus rings stay visible everywhere (system Highlight), severity
   swatches, strips, timeline bars, and map markers stay distinguishable
   (they keep their verified palette), and charts remain legible on their
   themed card surface.

## Deliberate exceptions

- `jsx-a11y/prefer-tag-over-role` is off: the dashboard uses
  `role="status"` for live regions, `role="group"` for toolbars, and
  `role="img"` for labelled inline SVG — correct ARIA that the rule's
  preferred tags (`output`, `fieldset`, `img`) would misrepresent. The
  rationale lives next to the setting in `web/.oxlintrc.json`.
- `views/Login.tsx` suppresses `jsx-a11y/no-autofocus` once: the login
  form is the page's single purpose, so focusing its first field does not
  disorient.
- `components/DataTable.tsx` suppresses two interaction rules once for
  the focus-capture bookkeeping on the table region — a listener, not an
  interaction; the rules cannot tell the difference.
- `PathGraph.tsx` labels its SVG in fixed 12px user units — below the
  0.75rem floor as source text, but the units scale with the viewBox (and
  with page zoom), and fixed units keep text measurement and node
  rectangles in one coordinate system at non-default font settings.
- uPlot renders chart axis text to canvas at its default pixel size, out
  of CSS reach. Page zoom (the mechanism that matters for canvas) scales
  it correctly, and the plotted data is equally available at rem sizes
  through the HTML legend, the sr-only series digest, and the keyboard
  investigation readout.
- The operations map's dot-matrix grid sits below 3:1 in both themes by
  intent: it is a decorative backdrop texture, not information — sites,
  selection, and severity all render as separate ≥3:1 marks above it.
- `--series-a` and `--series-b` sit near 1:1 against *each other*.
  Direction identity never rests on that adjacent edge: each series holds
  ≥3:1 against the surface, and the pairing is CVD-validated and always
  accompanied by the legend, per-direction labels, and the keyboard
  readouts from #105.
- Under forced colors, marks whose color *is* the data — legend swatches,
  fleet strips, sign-in bars, the incident timeline, the map surface with
  its legend and hover tip, and the canvas-backed chart cards — opt out
  via `forced-color-adjust: none` and keep the AA-verified palette (the
  canonical sanctioned use of the property). Each island also pins its
  themed `--surface` backdrop, because the forced Canvas can sit at the
  opposite luminance from the active theme; swatches and map bubbles
  additionally take `CanvasText` borders/strokes so they stay bounded on
  any forced page. Everything else, focus indicators included, rides the
  system palette.

## Visualization patterns

Issue #105 removed the last pointer-only surfaces with three deliberate
patterns, pinned by the source tests:

- **Slot grids** (health strips, incident timeline): the svg is decorative
  (`aria-hidden`) under a `role="group"` wrapper carrying the aggregate
  label; each slot is a real transparent `<button>` layered over it with a
  roving tabindex, so a table of strips stays one tab stop each. Hover and
  focus drive the same `role="status"` readout card.
- **Focusable pan/zoom surfaces** (chart hosts, the map svg): keyboard and
  pointer listeners attach natively in effects — the same idiom
  `DisclosureMenu.tsx` documents — because a noninteractive element cannot
  carry delegated JSX handlers without tripping the lint rules it should
  trip on real gaps. Charts expose their plotted series as an sr-only
  per-series digest of the visible range (not a live region; zoom changes
  are announced through the existing status span). The map is a labelled
  `role="img"` with its site markers as HTML buttons layered above,
  its key bindings named in a visible hint the svg is `aria-describedby`-
  linked to, and its zoom announcer debounced so wheel and pinch streams
  announce only the settled value. Focus-driven readout cards may announce
  after the button's own label; that double announcement is accepted.
- **Hover-only readouts** (the users panel's sign-ins chart): the svg
  keeps its aggregate `role="img"` label and pairs the pointer readout
  with an sr-only per-month breakdown instead of synthetic tab stops —
  there is no action to activate.

## Contrast tokens

Issue #106 measured every token pair the dashboard composes, fixed the
failures (light `--ink-3`, `--status-ok`, `--status-stale`, the fleet
strip's no-data fill, and form-control borders), and pinned the result:
the table below is recomputed from `web/src/styles.css` by
`a11y-source.test.mjs` on every test run, so a token edit that breaks a
bar fails CI with the failing pair named. Text roles need 4.5:1
(WCAG 1.4.3); non-text marks need 3:1 (WCAG 1.4.11). Ratios are WCAG
relative-luminance contrast; `color-mix` backgrounds are resolved in
srgb gamma space exactly as browsers resolve them.

Mind the two thin margins — light `--status-crit` on `--surface` (4.53)
and dark `--status-ok` on `--status-ok-bg` (4.52): any change to a
surface or a status-bg mix percentage must re-clear them.

| Pair (fg on bg) | Bar | Light | Dark |
|---|---|---|---|
| `--ink` on `--bg` | 4.5:1 | 16.55 | 18.00 |
| `--ink` on `--surface` | 4.5:1 | 17.72 | 16.73 |
| `--ink` on `--surface-2` | 4.5:1 | 16.12 | 15.28 |
| `--ink-2` on `--bg` | 4.5:1 | 7.22 | 9.83 |
| `--ink-2` on `--surface` | 4.5:1 | 7.73 | 9.13 |
| `--ink-2` on `--surface-2` | 4.5:1 | 7.03 | 8.35 |
| `--ink-3` on `--bg` | 4.5:1 | 4.86 | 7.72 |
| `--ink-3` on `--surface` | 4.5:1 | 5.20 | 7.17 |
| `--ink-3` on `--surface-2` | 4.5:1 | 4.74 | 6.56 |
| `--accent-ink` on `--accent` | 4.5:1 | 6.29 | 6.28 |
| `--accent` on `--surface` | 4.5:1 | 6.29 | 6.16 |
| `--accent` on `--bg` | 4.5:1 | 5.87 | 6.63 |
| `--status-ok` on `--surface` | 4.5:1 | 5.90 | 5.48 |
| `--status-ok` on `--status-ok-bg` | 4.5:1 | 5.13 | 4.52 |
| `--status-degraded` on `--surface` | 4.5:1 | 5.54 | 11.43 |
| `--status-degraded` on `--status-degraded-bg` | 4.5:1 | 5.02 | 8.77 |
| `--status-down` on `--surface` | 4.5:1 | 6.52 | 6.65 |
| `--status-down` on `--status-down-bg` | 4.5:1 | 5.47 | 5.75 |
| `--status-stale` on `--surface` | 4.5:1 | 5.21 | 5.22 |
| `--status-stale` on `--status-stale-bg` | 4.5:1 | 4.74 | 4.77 |
| `--status-warn` on `--surface` | 4.5:1 | 5.54 | 11.43 |
| `--status-crit` on `--surface` | 4.5:1 | 4.53 | 5.92 |
| `--focus` on `--bg` | 3:1 | 4.17 | 6.63 |
| `--focus` on `--surface` | 3:1 | 4.47 | 6.16 |
| `--series-a` on `--surface` | 3:1 | 4.42 | 5.05 |
| `--series-b` on `--surface` | 3:1 | 3.94 | 4.66 |
| `--chart-local` on `--surface` | 3:1 | 6.29 | 5.82 |
| `--chart-oidc` on `--surface` | 3:1 | 3.94 | 4.66 |
| `--strip-nodata` on `--surface` | 3:1 | 3.71 | 3.64 |
| `--strip-nodata` on `--surface-2` | 3:1 | 3.37 | 3.33 |
| `--control-border` on `--surface` | 3:1 | 3.42 | 3.64 |
| `--control-border` on `--surface-2` | 3:1 | 3.11 | 3.33 |

Light darkens `--status-ok` and `--status-stale` for text duty on white;
the dark block overrides them back to the brighter originals suited to
near-black surfaces. Below-bar values that remain are the documented
exceptions above (map dot grid, series-vs-series adjacency).

## Conformance reporting

When a formal Accessibility Conformance Report (ACR/VPAT) is needed,
produce it with the OpenACR Editor (https://acreditor.section508.gov/)
against the WCAG 2.1 AA target stated above.
