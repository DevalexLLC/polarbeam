import { useEffect, useRef, useState, type ReactNode } from 'react'
import uPlot from 'uplot'
import { reconcileChartRange, xExtent, type ChartRange } from '../chartRange'

// One investigation-aware uPlot lifecycle shared by pair, target-source,
// and stage charts. The selected x range is absolute so polling can append
// data without moving it; contextKey names every input that must reset it.
export default function Chart({
  options,
  data,
  contextKey,
  empty,
}: {
  options: Omit<uPlot.Options, 'width'>
  data: uPlot.AlignedData
  contextKey: string
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
    const onDoubleClick = () => {
      if (!selectedRef.current) return
      selectedRef.current = null
      setSelectedRange(null)
      setMode('live')
      setAnnouncement('Returned to live data.')
      plot.setData(dataRef.current)
    }
    host.addEventListener('mousedown', onMouseDown)
    host.addEventListener('dblclick', onDoubleClick)
    window.addEventListener('mouseup', onMouseUp)
    return () => {
      host.removeEventListener('mousedown', onMouseDown)
      host.removeEventListener('dblclick', onDoubleClick)
      window.removeEventListener('mouseup', onMouseUp)
      ro.disconnect()
      plot.destroy()
      plotRef.current = null
    }
  }, [contextKey, options])

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
      {empty}
      <div
        ref={hostRef}
        className={'chart-host' + (empty ? ' chart-host-empty' : '')}
        aria-hidden={empty ? true : undefined}
      />
    </div>
  )
}
