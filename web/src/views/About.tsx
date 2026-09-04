import LogoMark from '../components/LogoMark'

// Deliberately link-free. PolarBEAM targets air-gapped installs, where a
// link to github.com is dead on arrival — so the page names the on-disk
// locations of the license texts instead. Please do not "fix" these into
// hyperlinks.
const LICENSE_LOCATIONS = [
  { where: 'Container images', path: '/licenses' },
  { where: 'Release bundle', path: 'beside the compose file' },
]

export default function About({ version }: { version: string }) {
  return (
    <>
      <div className="page-head page-head-primary">
        <div>
          {/* The mark rides the page title so the product is named once, at
              the top; the cards below carry their own section headings. */}
          <h1 className="about-title">
            <LogoMark className="logo-mark about-mark" />
            PolarBEAM
          </h1>
          <p>
            Inter-site connectivity monitoring — latency, loss, jitter, and path changes, measured in both directions.
          </p>
        </div>
      </div>

      <div className="card about-card">
        <div className="card-head">
          <h2>This installation</h2>
        </div>
        <dl className="about-facts">
          <div>
            <dt>Server version</dt>
            <dd className="mono">{version || '—'}</dd>
          </div>
          <div>
            <dt>License</dt>
            <dd>AGPL-3.0-only</dd>
          </div>
          <div>
            {/* The label carries the sense; a © here would read "copyright
                copyright". The glyph belongs on the login byline, which has
                no label. */}
            <dt>Copyright</dt>
            <dd>2026 Devalex LLC</dd>
          </div>
        </dl>
      </div>

      <div className="card about-card">
        <div className="card-head">
          <h2>Third-party software</h2>
        </div>
        <p className="about-note">
          This product includes third-party software. The full license and attribution texts — <code>LICENSE</code>,{' '}
          <code>NOTICE</code>, and <code>THIRD-PARTY-NOTICES</code> — ship with every release artifact:
        </p>
        <dl className="about-facts">
          {LICENSE_LOCATIONS.map((loc) => (
            <div key={loc.where}>
              <dt>{loc.where}</dt>
              <dd className="mono">{loc.path}</dd>
            </div>
          ))}
        </dl>
      </div>
    </>
  )
}
