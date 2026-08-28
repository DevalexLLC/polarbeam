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
  adding a new defect fails the test. Contrast-token assertions join the
  file once the measured token table from issue #106 lands.

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
3. **Contrast spot checks.** New or changed color pairs meet 4.5:1 for
   text and 3:1 for non-text indicators in both themes (WebAIM contrast
   checker or equivalent).
4. **Text scaling.** At 200% browser text size and 400% page zoom the
   changed views lose no content or functionality and body content does
   not scroll horizontally.

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

## Conformance reporting

When a formal Accessibility Conformance Report (ACR/VPAT) is needed,
produce it with the OpenACR Editor (https://acreditor.section508.gov/)
against the WCAG 2.1 AA target stated above.
