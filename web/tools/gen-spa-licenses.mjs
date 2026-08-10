// Regenerate web/THIRD-PARTY-LICENSES from the SPA's PRODUCTION dependency
// closure.
//
// Why a committed file: the built bundle in web/dist is embedded into
// polarbeam-server (web/embed.go), so React et al. are redistributed inside
// every server binary and their MIT notices must travel with it. But
// node_modules is gitignored and absent from the offline build, so the
// Go-side generator (tools/gen-third-party-notices.sh) cannot read it. This
// script runs where node already runs — `make web`, alongside the dist
// rebuild it must stay in lockstep with — and commits its output for the
// offline generator to fold in.
//
// Scope is the production closure PLUS the build tools that emit their own
// runtime code into the output. Most tooling (typescript, oxlint, oxfmt)
// ships in nothing and is excluded, but two toolchain packages inject
// verbatim runtime helpers into every production bundle, so their
// MIT-licensed code is redistributed inside web/dist and inside the server
// binary that embeds it:
//
//   vite      — the modulepreload polyfill
//   rolldown  — the __commonJSMin CommonJS interop helper (Vite 8's bundler;
//               a transitive dep of vite, not a direct devDependency)
//
// This CANNOT be derived from the dependency graph: both are dev-side, and
// including the whole toolchain closure instead would attribute packages
// (postcss, picomatch, …) whose code is provably not in the bundle — a
// false claim in a legal document. So the list is curated, and swapping or
// upgrading the bundler is exactly the change that adds to it. Verify
// against the head of web/dist/assets/index-*.js when the pipeline moves.
//
// Fail loud (project constraint): a bundled package with no license file
// aborts rather than silently shipping unattributed code.
import { execFileSync } from 'node:child_process'
import { readFileSync, readdirSync, realpathSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

const LICENSE_RE = /^(LICENSE|LICENCE|COPYING)(\..*)?$/i

// Build-time packages whose own code is emitted into the bundle. `from`
// names the parent to resolve through for packages that are not direct
// dependencies (pnpm symlinks a package's deps beside it in the store).
const EMITTED_BY_BUILD = [{ name: 'vite' }, { name: 'rolldown', from: 'vite' }]

const raw = execFileSync('pnpm', ['list', '--prod', '--depth', 'Infinity', '--json'], {
  encoding: 'utf8',
  maxBuffer: 32 * 1024 * 1024,
})

const found = new Map()
const walk = (deps) => {
  for (const [name, info] of Object.entries(deps ?? {})) {
    if (info.path) found.set(`${name}@${info.version}`, { name, version: info.version, path: info.path })
    walk(info.dependencies)
  }
}
for (const project of JSON.parse(raw)) walk(project.dependencies)

if (found.size === 0) {
  console.error('spa-licenses: FATAL — no production dependencies resolved; run pnpm install first')
  process.exit(1)
}

const resolved = new Map()
for (const { name, from } of EMITTED_BY_BUILD) {
  // Direct devDependencies are symlinked at the top level; transitive ones
  // live beside their parent inside pnpm's store, reachable only through the
  // parent's real path.
  const path = from ? join(dirname(realpathSync(resolved.get(from))), name) : join('node_modules', name)
  let version
  try {
    version = JSON.parse(readFileSync(join(path, 'package.json'), 'utf8')).version
  } catch {
    console.error(`spa-licenses: FATAL — ${name} is listed as emitted into the bundle but was not found at ${path}`)
    process.exit(1)
  }
  resolved.set(name, path)
  found.set(`${name}@${version}`, { name, version, path })
}

const out = [
  'PolarBEAM dashboard — bundled third-party licenses',
  '===================================================',
  '',
  'GENERATED FILE — do not edit by hand. Regenerate with `make web`',
  '(or node tools/gen-spa-licenses.mjs from web/).',
  '',
  'These packages are bundled into web/dist, which is embedded into the',
  'polarbeam-server binary: the runtime dependencies, plus the build tools',
  '(vite, rolldown) that inject their own runtime helpers into every',
  'production bundle. Tooling that emits no code into the output is',
  'deliberately excluded.',
  '',
]

const missing = []
for (const key of [...found.keys()].toSorted()) {
  const { name, version, path } = found.get(key)
  const file = readdirSync(path)
    .filter((f) => LICENSE_RE.test(f))
    .toSorted()[0]
  if (!file) {
    missing.push(key)
    continue
  }
  out.push(
    '',
    '-'.repeat(80),
    `${name} ${version}`,
    '-'.repeat(80),
    '',
    `[${file}]`,
    '',
    readFileSync(join(path, file), 'utf8').trimEnd(),
  )
}

if (missing.length > 0) {
  console.error('spa-licenses: FATAL — bundled package(s) with no license file:')
  for (const m of missing) console.error(`  ${m}`)
  process.exit(1)
}

writeFileSync('THIRD-PARTY-LICENSES', out.join('\n') + '\n')
console.log(`spa-licenses: wrote web/THIRD-PARTY-LICENSES (${found.size} packages)`)
