// Reads the styles root and concatenates its @import partials in order, so
// the source-pin tests keep running against the full stylesheet exactly as
// the browser cascades it. A partial added to styles.css is covered
// automatically; a rule parked in the root file (where the bundle would
// miss it) or a missing partial fails loudly.
import { readFileSync } from 'node:fs'

const rootUrl = new URL('../../src/styles.css', import.meta.url)

export function readStyles() {
  const root = readFileSync(rootUrl, 'utf8')
  const imports = [...root.matchAll(/^@import '(\.\/styles\/[^']+\.css)';$/gm)].map((m) => m[1])
  if (imports.length === 0) {
    throw new Error('web/src/styles.css declares no @import partials; test/util/styles.mjs is out of sync')
  }
  const residue = root
    .replaceAll(/\/\*[\s\S]*?\*\//g, '')
    .replaceAll(/^@import '[^']+';$/gm, '')
    .trim()
  if (residue !== '') {
    throw new Error(
      `web/src/styles.css must hold only comments and @import lines so the test bundle stays complete; found: ${residue.slice(0, 80)}`,
    )
  }
  return imports.map((rel) => readFileSync(new URL(rel, rootUrl), 'utf8')).join('\n')
}
