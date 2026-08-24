import { apiGet } from '../api'
import type { PathThresholdOverride, SettingsResponse, ThresholdSettings } from '../types'
import ThresholdOverrideForm from './ThresholdOverrideForm'

// The plane rides as ?network=; '' addresses the all-planes row that
// predates tenancy. Omitting it on a plane-qualified row would silently
// edit (or delete) the all-planes row instead — a different row that grades
// every other tenant.
export function pathThresholdURL(a: string, b: string, network = ''): string {
  const base = `/api/v1/settings/path-thresholds/${encodeURIComponent(a)}/${encodeURIComponent(b)}`
  return network === '' ? base : `${base}?network=${encodeURIComponent(network)}`
}

// One pair's threshold override, hosted by the Settings → Thresholds
// overrides table (edit rows and the add flow). All the form behaviour is
// shared with the per-network defaults; this only resolves which row is
// being addressed.
export default function PathThresholdEditor({
  a,
  b,
  network = '',
  override,
  global,
  canWrite,
  onCancel,
  onChanged,
  onAuthError,
  onDirtyChange,
}: {
  a: string
  b: string
  // The plane this row belongs to; '' is the all-planes row.
  network?: string
  override: PathThresholdOverride | null
  // What an empty field falls back to: the plane's defaults folded over the
  // global row, already resolved by the panel.
  global: ThresholdSettings
  canWrite: boolean
  onCancel?: () => void
  onChanged: (warnings: string[]) => void
  onAuthError: (err: unknown) => void
  onDirtyChange?: (dirty: boolean) => void
}) {
  return (
    <ThresholdOverrideForm
      url={pathThresholdURL(a, b, network)}
      resource={`Path threshold override for ${a} ↔ ${b}${network ? ` on ${network}` : ''}`}
      override={override}
      inherited={global}
      canWrite={canWrite}
      emptyHint={network === '' ? 'the global thresholds' : `the ${network} defaults`}
      onCancel={onCancel}
      onChanged={onChanged}
      onAuthError={onAuthError}
      loadLatest={async () => {
        const latest = await apiGet<SettingsResponse>('/api/v1/settings')
        return (
          latest.overrides.find(
            (item) => item.network === network && ((item.a === a && item.b === b) || (item.a === b && item.b === a)),
          ) ?? null
        )
      }}
      onDirtyChange={onDirtyChange}
    />
  )
}
