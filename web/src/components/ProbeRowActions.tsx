import { ApiError, apiDelete, apiGet, apiPut } from '../api'
import { fmtAgo } from '../format'
import { useSettingsMutation } from '../settingsMutation'
import { serverSnapshotChanged } from '../settingsSnapshot'
import type { ProbeConfig } from '../types'
import { assignmentLabel, mutationSnapshot, paramsSummary } from '../probeDraft'
import ConfirmButton from './ConfirmButton'
import { type DataTableColumn } from './DataTable'

// The probes DataTable columns live beside the row actions so everything
// row-shaped about the probes inventory is in one place.
export function probeColumns(multiNetwork: boolean): DataTableColumn<ProbeConfig>[] {
  return [
    {
      key: 'type',
      label: 'Type',
      sortKey: 'type',
      priority: 'status',
      className: 'mono',
      render: (probe) => probe.type,
    },
    {
      key: 'assignment',
      label: 'Assignment',
      sortKey: 'site',
      priority: 'identity',
      className: 'mono',
      render: (probe) => (
        <>
          {assignmentLabel(probe)}
          {multiNetwork && <span className="chip">{probe.network}</span>}
        </>
      ),
    },
    {
      key: 'state',
      label: 'State',
      sortKey: 'enabled',
      priority: 'primary',
      render: (probe) => (
        <span className={'chip' + (probe.enabled ? '' : ' chip-alert')}>{probe.enabled ? 'enabled' : 'disabled'}</span>
      ),
    },
    { key: 'interval', label: 'Interval', priority: 'primary', render: (probe) => `${probe.interval_ms / 1000}s` },
    { key: 'timeout', label: 'Timeout', priority: 'secondary', render: (probe) => `${probe.timeout_ms / 1000}s` },
    {
      key: 'params',
      label: 'Params',
      priority: 'secondary',
      className: 'mono config-params-cell',
      render: paramsSummary,
    },
    {
      key: 'updated',
      label: 'Updated',
      sortKey: 'updated',
      priority: 'secondary',
      render: (probe) => (
        <>
          {fmtAgo(probe.updated_at)}
          {probe.updated_by ? ` by ${probe.updated_by}` : ''}
        </>
      ),
    },
  ]
}

// The Edit / Disable / Enable / Delete buttons rendered inside a row's
// floating actions menu, together with the enable-toggle and delete
// mutations they trigger. Editing itself stays with the panel: opening
// the editor is route- and focus-choreography the parent owns via onEdit.
export default function ProbeRowActions({
  probe,
  busy,
  onBusyChange,
  onRowError,
  onWarnings,
  onEdit,
  onRemoved,
  onRefresh,
  onAuthError,
}: {
  probe: ProbeConfig
  busy: boolean
  onBusyChange: (busy: boolean) => void
  onRowError: (message: string) => void
  onWarnings: (warnings: string[]) => void
  onEdit: () => void
  onRemoved: (p: ProbeConfig) => void
  onRefresh: () => Promise<void>
  onAuthError: (err: unknown) => void
}) {
  const feedback = useSettingsMutation()

  const setEnabled = async (p: ProbeConfig, nextEnabled: boolean) => {
    onBusyChange(true)
    onRowError('')
    try {
      let latest: ProbeConfig
      try {
        latest = await apiGet<ProbeConfig>('/api/v1/config/probes/' + p.id)
      } catch (requestError) {
        if (requestError instanceof ApiError && requestError.status === 404) {
          feedback.conflict(`Probe ${assignmentLabel(p)}`, () => void onRefresh())
          return
        }
        throw requestError
      }
      if (serverSnapshotChanged(mutationSnapshot(p), mutationSnapshot(latest))) {
        feedback.conflict(`Probe ${assignmentLabel(p)}`, () => void onRefresh())
        return
      }
      const res = await apiPut<{ warnings?: string[] }>('/api/v1/config/probes/' + p.id, {
        ...mutationSnapshot(latest),
        enabled: nextEnabled,
      })
      // Re-enabling a probe configured before the advisory existed is the
      // one moment an upgraded installation hears about it, so this write
      // reports warnings like the others.
      onWarnings(res.warnings ?? [])
      feedback.success(nextEnabled ? 'Probe enabled.' : 'Probe disabled.')
      await onRefresh()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      onRowError(message)
      feedback.error(`Probe state was not changed: ${message}`)
    } finally {
      onBusyChange(false)
    }
  }

  const remove = async (p: ProbeConfig) => {
    onBusyChange(true)
    onRowError('')
    try {
      await apiDelete('/api/v1/config/probes/' + p.id)
      onRemoved(p)
      feedback.success('Probe deleted.')
      await onRefresh()
    } catch (err) {
      onAuthError(err)
      const message = err instanceof Error ? err.message : String(err)
      onRowError(message)
      feedback.error(`Probe was not deleted: ${message}`)
    } finally {
      onBusyChange(false)
    }
  }

  return (
    <>
      <button type="button" className="secondary-button" disabled={busy} onClick={onEdit}>
        Edit
      </button>
      {probe.enabled ? (
        <ConfirmButton
          label="Disable"
          resource={`Probe ${assignmentLabel(probe)}`}
          consequence="This stops new measurements and may close incidents that depend on this probe."
          disabled={busy}
          onConfirm={() => setEnabled(probe, false)}
        />
      ) : (
        <button type="button" className="secondary-button" disabled={busy} onClick={() => setEnabled(probe, true)}>
          Enable
        </button>
      )}
      <ConfirmButton
        label="Delete"
        resource={`Probe ${assignmentLabel(probe)}`}
        consequence={
          probe.mesh
            ? 'This removes every expanded pair workload and retires their measurement series.'
            : 'This permanently removes the probe and retires its measurement series.'
        }
        disabled={busy}
        onConfirm={() => remove(probe)}
      />
    </>
  )
}
