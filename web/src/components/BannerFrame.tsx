import type { ReactNode } from 'react'
import type { UIBanner } from '../types'

// BannerFrame wraps every top-level screen (boot, login, app) in the
// admin-configured marking bands. The wrapper div is always rendered and
// only its class toggles: swapping the element type (fragment vs div) when
// a poll flips `enabled` would remount the whole subtree and discard
// in-progress input. All banner CSS is scoped under .banner-frame, so the
// classless div is layout-inert.
export default function BannerFrame({ banner, children }: { banner: UIBanner | null; children: ReactNode }) {
  const active = (banner?.enabled ?? false) && banner?.text !== ''
  return (
    <div className={active ? 'banner-frame' : undefined}>
      {active && (
        <div className="ui-banner ui-banner-top" role="note">
          {banner?.text}
        </div>
      )}
      {children}
      {/* Same text twice; hide the duplicate from the accessibility tree. */}
      {active && (
        <div className="ui-banner ui-banner-bottom" aria-hidden="true">
          {banner?.text}
        </div>
      )}
    </div>
  )
}
