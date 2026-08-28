import { useEffect, useRef, useState, type ReactNode } from 'react'
import uPlot from 'uplot'
import { panChartRange, reconcileChartRange, xExtent, zoomChartRange, type ChartRange } from '../chartRange'
import { summarizeSeries } from '../chartkit'
import { getTZMode, useTimezone } from '../timezone'

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

// x values are epoch seconds; announcements and the sr-only summary label
// ranges as HH:MM in the operator's chosen timezone (timezone.ts). The
// date joins when the endpoints fall on different calendar days (a 24 h
// window crosses midnight even at a span just under a day, and bare
// "18:04–18:03" reads as a reversed range); seconds join on narrow
// ranges, where consecutive zoom/pan steps could otherwise produce the
// identical minute string and a live region that never re-fires.
function sameCalendarDay(aS: number, bS: number, utc: boolean): boolean {
  const a = new Date(aS * 1000)
  const b = new Date(bS * 1000)
  if (utc) {
    return (
      a.getUTCFullYear() === b.getUTCFullYear() &&
      a.getUTCMonth() === b.getUTCMonth() &&
      a.getUTCDate() === b.getUTCDate()
    )
  }
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate()
}

const pad2 = (n: number) => String(n).padStart(2, '0')

function rangeClock(epochS: number, utc: boolean, withDate: boolean, withSeconds: boolean): string {
  const d = new Date(epochS * 1000)
  if (utc) {
    const hm = `${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}${withSeconds ? `:${pad2(d.getUTCSeconds())}` : ''}`
    return withDate ? `${MONTHS[d.getUTCMonth()]} ${d.getUTCDate()} ${hm}` : hm
  }
  return d.toLocaleString([], {
    ...(withDate ? { month: 'short', day: 'numeric' } : {}),
    hour: '2-digit',
    minute: '2-digit',
    ...(withSeconds ? { second: '2-digit' } : {}),
  })
}

function clockRange(range: ChartRange, utc: boolean): string {
  const withDate = !sameCalendarDay(range.min, range.max, utc)
  const withSeconds = range.max - range.min < 600
  return `${rangeClock(range.min, utc, withDate, withSeconds)}–${rangeClock(range.max, utc, withDate, withSeconds)}${utc ? ' UTC' : ''}`
}

const fmtValue = (v: number) => (Math.abs(v) >= 100 ? Math.round(v).toString() : v.toFixed(1))

// One investigation-aware uPlot lifecycle shared by pair, target-source,
// and stage charts. The selected x range is absolute so polling can append
// data without moving it; contextKey names every input that must reset it.
export default function Chart({
  options,
  data,
  contextKey,
  label = 'Chart',
  empty,
}: {
  options: Omit<uPlot.Options, 'width'>
  data: uPlot.AlignedData
  contextKey: string
  label?: string
  empty?: ReactNode
}) {
  const hostRef = useRef<HTMLDivElement>(null)
  const plotRef = useRef<uPlot | null>(null)
  const dataRef = useRef(data)
  const selectedRef = useRef<ChartRange | null>(null)
  const contextRef = useRef<string | null>(null)
  const dragRef = useRef(false)
  const [mode, setMode] = useState<'live' | 'zoomed'>('live')
  const [selectedRange, setSelectedRange] = useState<ChartRange | null>(null)
  const [announcement, setAnnouncement] = useState('')
  const { mode: tzMode } = useTimezone()
  dataRef.current = data

  useEffect(() => {
    const host = hostRef.current
    if (!host) return
    const contextChanged = contextRef.current !== contextKey
    contextRef.current = contextKey
    const reconciliation = reconcileChartRange(selectedRef.current, xExtent(dataRef.current[0]), contextChanged)
    selectedRef.current = reconciliation.range
    setMode(reconciliation.mode)
    setSelectedRange(reconciliation.range)
    if (contextChanged) setAnnouncement('')
    const plot = new uPlot({ ...options, width: Math.max(host.clientWidth, 200) }, dataRef.current, host)
    plotRef.current = plot
    if (reconciliation.range) plot.setScale('x', reconciliation.range)
    const ro = new ResizeObserver(() => {
      plot.setSize({ width: Math.max(host.clientWidth, 200), height: options.height })
    })
    ro.observe(host)
    const onMouseDown = (event: MouseEvent) => {
      if (event.button === 0) dragRef.current = true
    }
    const onMouseUp = () => {
      if (!dragRef.current) return
      dragRef.current = false
      requestAnimationFrame(() => {
        if (plotRef.current !== plot) return
        const min = plot.scales.x.min
        const max = plot.scales.x.max
        const extent = xExtent(plot.data[0])
        if (min == null || max == null || !extent || max <= min) return
        const epsilon = Math.max((extent.max - extent.min) * 0.000_001, Number.EPSILON)
        if (min <= extent.min + epsilon && max >= extent.max - epsilon) return
        selectedRef.current = { min, max }
        setSelectedRange({ min, max })
        setMode('zoomed')
        setAnnouncement('')
      })
    }
    const reset = (announce: string) => {
      selectedRef.current = null
      setSelectedRange(null)
      setMode('live')
      setAnnouncement(announce)
      plot.setData(dataRef.current)
    }
    const onDoubleClick = () => {
      if (!selectedRef.current) return
      reset('Returned to live data.')
    }
    // Keyboard investigation path, mirroring the drag/dblclick gestures:
    // the range math is pure (chartRange.ts) and reads the plot's current
    // data at key time, so polling never invalidates a binding.
    const applyRange = (range: ChartRange) => {
      selectedRef.current = range
      setSelectedRange(range)
      setMode('zoomed')
      plot.setScale('x', range)
      setAnnouncement(`Zoomed to ${clockRange(range, getTZMode() === 'utc')}.`)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.ctrlKey || event.metaKey || event.altKey) return
      const extent = xExtent(plot.data[0])
      const current = selectedRef.current
      switch (event.key) {
        case '+':
        case '=': {
          const base = current ?? extent
          if (!base) return
          // A preserved range wider than the retained extent can halve
          // and still cover everything (null); zoom into the extent
          // itself then, so + always makes progress back to the data.
          const next = zoomChartRange(base, 0.5, extent) ?? (extent ? zoomChartRange(extent, 0.5, extent) : null)
          if (next) applyRange(next)
          event.preventDefault()
          return
        }
        case '-':
        case '_': {
          if (!current) return
          const next = zoomChartRange(current, 2, extent)
          if (next) applyRange(next)
          else reset('Returned to live data.')
          event.preventDefault()
          return
        }
        case 'ArrowLeft':
        case 'ArrowRight': {
          if (!current) return
          applyRange(panChartRange(current, event.key === 'ArrowLeft' ? -0.2 : 0.2, extent))
          event.preventDefault()
          return
        }
        case 'Escape':
        case '0': {
          if (!current) return
          reset('Returned to live data.')
          event.preventDefault()
          return
        }
      }
    }
    host.addEventListener('mousedown', onMouseDown)
    host.addEventListener('dblclick', onDoubleClick)
    host.addEventListener('keydown', onKeyDown)
    window.addEventListener('mouseup', onMouseUp)
    return () => {
      host.removeEventListener('mousedown', onMouseDown)
      host.removeEventListener('dblclick', onDoubleClick)
      host.removeEventListener('keydown', onKeyDown)
      window.removeEventListener('mouseup', onMouseUp)
      ro.disconnect()
      plot.destroy()
      plotRef.current = null
    }
  }, [contextKey, options])

  // The host is the focusable keyboard surface, imperatively so the
  // noninteractive div carries no JSX tabIndex (the same effect-attached
  // idiom the listeners above already use). An empty chart is aria-hidden
  // and must not be a tab stop.
  useEffect(() => {
    const host = hostRef.current
    if (!host) return
    if (empty) host.removeAttribute('tabindex')
    else host.tabIndex = 0
  }, [empty])

  useEffect(() => {
    const plot = plotRef.current
    if (!plot) return
    const reconciliation = reconcileChartRange(selectedRef.current, xExtent(data[0]))
    selectedRef.current = reconciliation.range
    setSelectedRange(reconciliation.range)
    plot.setData(data)
    if (reconciliation.range) plot.setScale('x', reconciliation.range)
    if (reconciliation.reason === 'expired') {
      setMode('live')
      setAnnouncement('Selected range expired; returned to live data.')
    }
  }, [data])

  useEffect(() => {
    if (!announcement) return
    const timer = window.setTimeout(() => setAnnouncement(''), 4_000)
    return () => window.clearTimeout(timer)
  }, [announcement])

  const returnLive = () => {
    selectedRef.current = null
    setSelectedRange(null)
    setMode('live')
    setAnnouncement('Returned to live data.')
    plotRef.current?.setData(dataRef.current)
  }

  // Text alternative for the canvas: a per-series digest of the visible
  // range. Not a live region — polling would make it chatter; zoom changes
  // are announced through the status span instead.
  const utc = tzMode === 'utc'
  const visible = selectedRange ?? xExtent(data[0])
  const unit = typeof options.axes?.[1]?.label === 'string' ? options.axes[1].label : ''
  const summaryParts = summarizeSeries(data, selectedRange).map((s, i) => {
    const seriesLabel = options.series[i + 1]?.label
    const name = typeof seriesLabel === 'string' ? seriesLabel : `series ${i + 1}`
    if (s.count === 0) return `${name}: no data`
    return `${name}: latest ${fmtValue(s.latest!)}, minimum ${fmtValue(s.min!)}, maximum ${fmtValue(s.max!)}, average ${fmtValue(s.avg!)} over ${s.count} ${s.count === 1 ? 'point' : 'points'}`
  })

  return (
    <div className="chart-investigation" data-range-min={selectedRange?.min} data-range-max={selectedRange?.max}>
      {!empty && (
        <div className="chart-investigation-controls">
          <span className={'chip chart-investigation-mode' + (mode === 'zoomed' ? ' active' : '')}>
            {mode === 'zoomed' ? 'Zoomed' : 'Live'}
          </span>
          {mode === 'zoomed' && (
            <button type="button" className="secondary-button" onClick={returnLive}>
              Return live
            </button>
          )}
          <span className="hint" role="status" aria-live="polite">
            {announcement}
          </span>
        </div>
      )}
      {!empty && (
        <p className="sr-only">
          {label}
          {unit ? `, ${unit}` : ''}
          {visible ? `, showing ${clockRange(visible, utc)}` : ''}. {summaryParts.join('. ')}.
        </p>
      )}
      {empty}
      <div
        ref={hostRef}
        className={'chart-host' + (empty ? ' chart-host-empty' : '')}
        aria-hidden={empty ? true : undefined}
        role="group"
        aria-label={`${label}. Use the arrow keys to pan, plus and minus to zoom, Escape or 0 to return to live data.`}
      />
    </div>
  )
}
