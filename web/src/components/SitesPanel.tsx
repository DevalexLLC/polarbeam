import { useEffect, useRef, useState } from 'react'
import { apiDelete, apiGet, apiPost, apiPut } from '../api'
import { updateRouteParams } from '../routeState'
import { useRouteNumber, useRouteParam, useRouteSearch } from '../useRouteState'
import { fmtAgo } from '../format'
import type { SitesConfigResponse, SiteConfig } from '../types'
import ConfirmButton from './ConfirmButton'
import DataTable, { type DataTableColumn } from './DataTable'
import SettingsPageError from './SettingsPageError'

const POLL_MS = 30_000
const SITE_PAGE = 25

interface Draft {
  name: string
  display_name: string
  location: string
  latitude: string
  longitude: string
}

const emptyDraft: Draft = { name: '', display_name: '', location: '', latitude: '', longitude: '' }

// Mirrors the server's siteadmin validation; server 400s render verbatim as
// a backstop. Coordinates are both-or-neither — clearing both unplaces the
// site (the PUT is full-state).
function validate(d: Draft): { errors: string[]; latitude: number | null; longitude: number | null } {
  const errors: string[] = []
  if (d.name.trim() === '') errors.push('name is required')
  const hasLat = d.latitude.trim() !== ''
  const hasLon = d.longitude.trim() !== ''
  let latitude: number | null = null
  let longitude: number | null = null
  if (hasLat !== hasLon) {
    errors.push('latitude and longitude must be set together (clear both to unplace the site)')
  } else if (hasLat) {
    latitude = Number(d.latitude)
    longitude = Number(d.longitude)
    if (!Number.isFinite(latitude) || latitude < -90 || latitude > 90) {
      errors.push('latitude must be between -90 and 90')
      latitude = null
    }
    if (!Number.isFinite(longitude) || longitude < -180 || longitude > 180) {
      errors.push('longitude must be between -180 and 180')
      longitude = null
    }
  }
  return { errors, latitude, longitude }
}

function refCount(s: SiteConfig): number {
  return s.agent_count + s.mesh_count + s.probe_count
}

function refSummary(s: SiteConfig): string {
  return `${s.agent_count} agent(s), ${s.mesh_count} mesh membership(s), ${s.probe_count} probe config(s)`
}

export default function SitesPanel({
  canWrite,
  selectedSite,
  onSelectedSite,
  onAuthError,
}: {
  canWrite: boolean
  selectedSite: string
  onSelectedSite: (site: string, mode?: 'push' | 'replace') => void
  onAuthError: (err: unknown) => void
}) {
  const [data, setData] = useState<SitesConfigResponse | null>(null)
  const [loadedRequestURL, setLoadedRequestURL] = useState('')
  const [error, setError] = useState<unknown>(null)
  const [retryKey, setRetryKey] = useState(0)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [editing, setEditing] = useState(false) // draft edits an existing site (name locked)
  const [formErrors, setFormErrors] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)
  const [query, setQuery] = useRouteSearch()
  const [queryParam] = useRouteParam('q')
  const [sort] = useRouteParam('sort', 'name')
  const [order] = useRouteParam('order', 'asc')
  const [page, setPage] = useRouteNumber('page', 1)
  const [expandedRow, setExpandedRow] = useState<string | null>(null)
  const [actionRow, setActionRow] = useState<string | null>(null)
  const scrolledSite = useRef<string | null>(null)
  const pinnedSite = useRef<string | null>(selectedSite)

  if (!selectedSite) pinnedSite.current = null
  else if (pinnedSite.current !== selectedSite) {
    pinnedSite.current = data?.sites.some((site) => site.id === selectedSite) ? null : selectedSite
  }
  const pinnedSiteID = pinnedSite.current === selectedSite ? selectedSite : null

  const params = new URLSearchParams({
    limit: String(SITE_PAGE),
    offset: String(pinnedSiteID ? 0 : (page - 1) * SITE_PAGE),
    sort,
    order,
  })
  if (pinnedSiteID) params.set('q', pinnedSiteID)
  else if (queryParam.trim()) params.set('q', queryParam.trim())
  const requestURL = '/api/v1/config/sites?' + params.toString()

  useEffect(() => {
    let cancelled = false
    const load = () => {
      apiGet<SitesConfigResponse>(requestURL)
        .then((res) => {
          if (!cancelled) {
            setData(res)
            setLoadedRequestURL(requestURL)
            setError(null)
          }
        })
        .catch((err) => {
          if (cancelled) return
          onAuthError(err)
          console.error('site settings request failed', err)
          setError(err)
        })
    }
    load()
    const id = setInterval(load, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [onAuthError, requestURL, retryKey])

  const reload = () =>
    apiGet<SitesConfigResponse>(requestURL)
      .then((res) => {
        setData(res)
        setLoadedRequestURL(requestURL)
      })
      .catch(onAuthError)

  const save = async () => {
    if (!draft) return
    const { errors, latitude, longitude } = validate(draft)
    setFormErrors(errors)
    if (errors.length > 0) return
    setSaving(true)
    try {
      const body = {
        name: draft.name.trim(),
        display_name: draft.display_name.trim(),
        location: draft.location.trim(),
        latitude,
        longitude,
      }
      if (editing) {
        await apiPut('/api/v1/config/sites/' + encodeURIComponent(body.name), body)
      } else {
        await apiPost('/api/v1/config/sites', body)
      }
      setDraft(null)
      setEditing(false)
      onSelectedSite('', 'replace')
      setSavedFlash(true)
      await reload()
    } catch (err) {
      onAuthError(err)
      setFormErrors([err instanceof Error ? err.message : String(err)])
    } finally {
      setSaving(false)
    }
  }

  const remove = async (s: SiteConfig) => {
    try {
      await apiDelete('/api/v1/config/sites/' + encodeURIComponent(s.name))
      if (selectedSite === s.id) onSelectedSite('', 'replace')
      await reload()
    } catch (err) {
      onAuthError(err)
      console.error('site delete failed', err)
      setError(err)
    }
  }

  const startEdit = (s: SiteConfig) => {
    onSelectedSite(s.id)
    setEditing(true)
    setSavedFlash(false)
    setFormErrors([])
    setDraft({
      name: s.name,
      display_name: s.display_name,
      location: s.location,
      // 0 is a real coordinate — check null, never truthiness.
      latitude: s.latitude !== null ? String(s.latitude) : '',
      longitude: s.longitude !== null ? String(s.longitude) : '',
    })
  }

  useEffect(() => {
    if (!selectedSite) {
      scrolledSite.current = null
      if (editing) {
        setEditing(false)
        setDraft(null)
      }
      return
    }
    if (!data) return
    const selected = data.sites.find((site) => site.id === selectedSite)
    if (!selected) {
      if (pinnedSiteID && loadedRequestURL !== requestURL) return
      onSelectedSite('', 'replace')
      return
    }
    if (!editing || draft?.name !== selected.name) {
      setEditing(true)
      setSavedFlash(false)
      setFormErrors([])
      setDraft({
        name: selected.name,
        display_name: selected.display_name,
        location: selected.location,
        latitude: selected.latitude !== null ? String(selected.latitude) : '',
        longitude: selected.longitude !== null ? String(selected.longitude) : '',
      })
    }
    if (scrolledSite.current !== selectedSite) {
      const surface = window.matchMedia('(max-width: 760px)').matches ? 'mobile' : 'desktop'
      const row = document.getElementById(`settings-site-${selected.id}-${surface}`)
      if (!row) return
      row.scrollIntoView({ block: 'nearest' })
      scrolledSite.current = selectedSite
    }
  }, [data, draft?.name, editing, loadedRequestURL, onSelectedSite, pinnedSiteID, requestURL, selectedSite])

  const pageMeta = data?.page ?? { limit: SITE_PAGE, offset: 0, total: data?.sites.length ?? 0, has_more: false }
  const pageCount = Math.max(1, Math.ceil(pageMeta.total / SITE_PAGE))
  useEffect(() => {
    if (page > pageCount) setPage(pageCount, 'replace')
  }, [page, pageCount, setPage])

  if (error && !data) {
    return (
      <SettingsPageError
        title="Sites unavailable"
        subject="sites"
        error={error}
        onRetry={() => setRetryKey((key) => key + 1)}
      />
    )
  }
  if (!data) {
    return (
      <div className="state-panel" role="status">
        <span className="state-spinner" />
        Loading sites…
      </div>
    )
  }

  const columns: DataTableColumn<SiteConfig>[] = [
    {
      key: 'name',
      label: 'Name',
      sortKey: 'name',
      priority: 'identity',
      className: 'mono',
      render: (site) => site.name,
    },
    {
      key: 'display',
      label: 'Display name',
      sortKey: 'display_name',
      priority: 'primary',
      render: (site) => site.display_name || '—',
    },
    { key: 'location', label: 'Location', priority: 'primary', render: (site) => site.location || '—' },
    {
      key: 'coordinates',
      label: 'Coordinates',
      priority: 'secondary',
      className: 'mono',
      render: (site) =>
        site.latitude !== null && site.longitude !== null
          ? `${site.latitude.toFixed(4)}, ${site.longitude.toFixed(4)}`
          : '—',
    },
    { key: 'agents', label: 'Agents', sortKey: 'agents', priority: 'secondary', render: (site) => site.agent_count },
    { key: 'meshes', label: 'Meshes', sortKey: 'meshes', priority: 'secondary', render: (site) => site.mesh_count },
    { key: 'probes', label: 'Probes', sortKey: 'probes', priority: 'secondary', render: (site) => site.probe_count },
    {
      key: 'created',
      label: 'Created',
      sortKey: 'created',
      priority: 'secondary',
      render: (site) => fmtAgo(site.created_at),
    },
  ]

  const field = (label: string, key: keyof Draft, placeholder: string, locked = false) => (
    <label className="threshold-field">
      <span className="eyebrow">{label}</span>
      <span className="threshold-input">
        <input
          type="text"
          value={draft?.[key] ?? ''}
          placeholder={placeholder}
          disabled={saving || locked}
          onChange={(e) => {
            setSavedFlash(false)
            setDraft((d) => ({ ...(d ?? emptyDraft), [key]: e.target.value }))
          }}
        />
      </span>
    </label>
  )

  return (
    <>
      {error !== null && (
        <div className="inline-alert" role="status">
          Refresh failed. Showing the last successful snapshot.
        </div>
      )}
      <section className="card settings-card config-card">
        <div className="card-head">
          <div>
            <span className="eyebrow">Locations</span>
            <h2>Sites</h2>
          </div>
          <span className="hint">Refreshes every 30s</span>
        </div>
        <p className="section-intro">
          Agents enroll into a site; meshes and direct probes are assigned by site. A site referenced by agents, meshes,
          or probes cannot be deleted until those references are removed.
        </p>
        <div className="view-toolbar data-table-toolbar">
          <label className="search-field">
            <span className="sr-only">Search sites</span>
            <input
              type="search"
              placeholder="Search name or location"
              value={query}
              onChange={(event) => {
                setQuery(event.target.value)
                if (selectedSite) onSelectedSite('', 'replace')
              }}
            />
          </label>
        </div>
        <DataTable
          label="Sites"
          rows={data.sites}
          rowKey={(site) => site.id}
          rowID={(site) => 'settings-site-' + site.id}
          rowClassName={(site) => (selectedSite === site.id ? 'selected-row' : '')}
          columns={columns}
          sort={{ key: sort, order: order === 'desc' ? 'desc' : 'asc' }}
          onSortChange={(next) =>
            updateRouteParams({
              sort: next.key === 'name' ? null : next.key,
              order: next.order === 'asc' ? null : next.order,
              page: null,
              site: null,
            })
          }
          page={pageMeta}
          onPageChange={(next) => updateRouteParams({ page: next === 1 ? null : next, site: null })}
          resultLabel="sites"
          emptyTitle={query ? 'No matching sites' : 'No sites'}
          emptyDescription={
            query
              ? 'Change the search text.'
              : 'Add one below, then issue a join token from the Enrollment tab to enroll its first agent.'
          }
          disclosure={{
            expandedKey: expandedRow,
            onExpandedKeyChange: setExpandedRow,
            label: (_site, expanded) => (expanded ? 'Hide metadata' : 'Show metadata'),
            desktop: false,
          }}
          actions={
            canWrite
              ? {
                  openKey: actionRow,
                  onOpenKeyChange: setActionRow,
                  label: (site) => `Actions for ${site.name}`,
                  render: (site) => (
                    <>
                      <button
                        type="button"
                        className="secondary-button"
                        onClick={() => {
                          setActionRow(null)
                          startEdit(site)
                        }}
                      >
                        Edit
                      </button>
                      <ConfirmButton
                        label="Delete"
                        confirmLabel="Confirm delete? Unused join tokens go with it"
                        disabled={refCount(site) > 0}
                        title={refCount(site) > 0 ? `In use by ${refSummary(site)} — remove those first` : undefined}
                        onConfirm={() => remove(site)}
                      />
                    </>
                  ),
                }
              : undefined
          }
        />
        {canWrite && (
          <div className="config-form">
            <h3 className="eyebrow">{editing ? `Edit ${draft?.name}` : 'Add site'}</h3>
            <div className="config-form-grid">
              {field('Name', 'name', 'unique handle, e.g. nyc', editing)}
              {field('Display name', 'display_name', 'e.g. New York')}
              {field('Location', 'location', 'free text, e.g. New York, US')}
              {field('Latitude', 'latitude', '-90..90, with longitude')}
              {field('Longitude', 'longitude', '-180..180, with latitude')}
            </div>
            {formErrors.length > 0 && (
              <ul className="error threshold-errors">
                {formErrors.map((e) => (
                  <li key={e}>{e}</li>
                ))}
              </ul>
            )}
            <div className="threshold-foot">
              <span className="hint">
                Coordinates place the site on the Overview map. Clear both fields to remove it from the map.
              </span>
              <span className="threshold-actions">
                {savedFlash && <span className="hint">saved</span>}
                {(editing || draft) && (
                  <button
                    type="button"
                    className="secondary-button"
                    disabled={saving}
                    onClick={() => {
                      setDraft(null)
                      setEditing(false)
                      onSelectedSite('')
                      setFormErrors([])
                    }}
                  >
                    Cancel
                  </button>
                )}
                <button className="primary" onClick={save} disabled={saving || !draft}>
                  {saving ? 'Saving…' : editing ? 'Save changes' : 'Add site'}
                </button>
              </span>
            </div>
          </div>
        )}
      </section>
    </>
  )
}
