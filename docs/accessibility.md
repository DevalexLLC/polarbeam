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
   it to their trigger on close.
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
- The remaining suppressions (map, health strip, incident timeline, data
  table, users panel) are known keyboard/semantics gaps scheduled for
  issues #104 and #105; each is inventoried in the test baseline and
  justified by a comment at the suppression site.

## Conformance reporting

When a formal Accessibility Conformance Report (ACR/VPAT) is needed,
produce it with the OpenACR Editor (https://acreditor.section508.gov/)
against the WCAG 2.1 AA target stated above.
