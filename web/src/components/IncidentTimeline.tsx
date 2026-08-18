import { useMemo, useState } from 'react'
import { useTimezone } from '../timezone'
import type { OutageEvent, Window } from '../types'

// The incidents window divides into 48 buckets, whatever its span — the
// hit-target density HealthStrip proved out, with identical hover/click
// arithmetic across windows. The grid draws one extra slot: with epoch-
// aligned buckets, 48 alone would trim up to one bucket of history off the
// left edge (a week on 365d), hiding incidents the API returned.
export const SLOTS = 48
const GRID_SLOTS = SLOTS + 1

export const WINDOW_MS: Record<Window, number> = {
  '24h': 86_400_000,
  '7d': 7 * 86_400_000,
  '30d': 30 * 86_400_000,
  '90d': 90 * 86_400_000,
  '365d': 365 * 86_400_000,
}

export interface TimelineGrid {
  startMs: number
  endMs: number
  bucketMs: number
}

// The grid is epoch-aligned and includes the in-progress bucket, so it only
// shifts when a poll crosses a bucket boundary — never on a re-render.
// Callers pass fetch time as nowMs for the same reason. The 49 aligned
// slots always cover the full [nowMs - window, nowMs] span (the first and
// last slots are partially outside it; anchoring at nowMs instead would
// cover it exactly but shift every bucket start on every poll, breaking
// the time-addressed selection).
export function timelineGrid(win: Window, nowMs: number): TimelineGrid {
  const bucketMs = WINDOW_MS[win] / SLOTS
  const endMs = (Math.floor(nowMs / bucketMs) + 1) * bucketMs
  return { startMs: endMs - GRID_SLOTS * bucketMs, endMs, bucketMs }
}

// An incident occupies every bucket its lifetime overlaps: the server
// returns open events regardless of age, and an opened-in-bucket rule would
// hide a days-old active incident from the 24h view. The lifetime is
// half-open [opened_at, closed_at): closed_at stamps the first SUCCESSFUL
// sample, so the outage is not ongoing at that instant and a close landing
// exactly on a bucket boundary must not double-count into the next bucket.
// This predicate is the single source of truth for both the bars and the
// click-to-filter, so the chart and the groups card cannot disagree on
// what "in this slice" means.
export function overlapsBucket(e: OutageEvent, bucketStartMs: number, bucketMs: number, nowMs: number): boolean {
  // Agent-stamped times may run a few minutes ahead of the server clock
  // anchoring the grid (ingest tolerates future skew); clamp both ends to
  // the snapshot's now so a skew-stamped incident — open, or opened AND
  // resolved beyond the grid edge — lands in the current bucket instead
  // of falling off the grid's right edge.
  const closed = Math.min(e.closed_at ? Date.parse(e.closed_at) : nowMs, nowMs)
  const opened = Math.min(Date.parse(e.opened_at), closed)
  return opened < bucketStartMs + bucketMs && closed > bucketStartMs
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

// Label precision follows bucket width: times for 24h, date+time for the
// mid windows, dates once buckets span days. withZone only matters for the
// 24h local view, where a DST fall-back repeats a wall-clock hour (longer
// windows carry dates, which disambiguate on their own). withYear matters
// whenever the grid crosses a year boundary — always on 365d — where a
// bare "Aug 10" would silently mean last year.
function axisLabel(ms: number, win: Window, utc: boolean, withZone: boolean, withYear: boolean): string {
  const d = new Date(ms)
  if (win === '24h') {
    if (utc) return `${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}`
    return d.toLocaleTimeString(
      [],
      withZone ? { hour: '2-digit', minute: '2-digit', timeZoneName: 'short' } : { hour: '2-digit', minute: '2-digit' },
    )
  }
  const year = withYear && utc ? ` ${d.getUTCFullYear()}` : ''
  if (win === '365d') {
    if (utc) return `${MONTHS[d.getUTCMonth()]} ${d.getUTCDate()}${year}`
    return d.toLocaleDateString([], { month: 'short', day: 'numeric', ...(withYear ? { year: 'numeric' } : {}) })
  }
  if (utc)
    return `${MONTHS[d.getUTCMonth()]} ${d.getUTCDate()}${year} ${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}`
  return d.toLocaleString([], {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    ...(withYear ? { year: 'numeric' } : {}),
  })
}

// Shared by the hover card and the groups card's clear-filter chip so both
// describe the selected slice with the same words.
export function bucketRangeLabel(
  t: number,
  bucketMs: number,
  win: Window,
  utc: boolean,
  withZone = false,
  withYear = false,
): string {
  return `${axisLabel(t, win, utc, withZone, withYear)}–${axisLabel(t + bucketMs, win, utc, withZone, withYear)}${utc ? ' UTC' : ''}`
}

// True when the grid spans a year boundary (always on 365d), so labels can
// carry the year. UTC years are close enough for the local view — being a
// few hours early or late with a year tag is harmless.
export function gridWithYear(grid: TimelineGrid): boolean {
  return new Date(grid.startMs).getUTCFullYear() !== new Date(grid.endMs).getUTCFullYear()
}

// Local mode only: a UTC-offset change inside a 24h window means a DST
// transition — label times with the zone so a fall-back's repeated
// wall-clock hour stays distinguishable (longer windows carry dates, which
// disambiguate on their own). Exported so the clear-filter chip applies
// the same rule as the timeline's own labels.
export function gridWithZone(grid: TimelineGrid, win: Window, utc: boolean): boolean {
  return (
    !utc && win === '24h' && new Date(grid.startMs).getTimezoneOffset() !== new Date(grid.endMs).getTimezoneOffset()
  )
}

// Geometry in viewBox units (the SVG stretches to the card width).
const SLOT_W = 20
const BAR_W = 14
const CHART_H = 80
const BASELINE = CHART_H - 2
const MAX_BAR = CHART_H - 8

interface Slot {
  t: number
  openOffline: number
  openProbe: number
  resOffline: number
  resProbe: number
}

function slotTotal(s: Slot): number {
  return s.openOffline + s.openProbe + s.resOffline + s.resProbe
}

// The window's incidents as bucketed bars (hand-rolled SVG — uPlot is
// for real charts). Severity is derived from kind — agents offline are a
// harder failure than probes failing — since outage events carry no
// severity field of their own. Bars stack by kind, probe failures at the
// baseline and agent outages above, each kind solid (open) under muted
// (resolved).
// Heights use sqrt scaling: outage counts are heavy-tailed, and one
// agent-offline cascade would flatten every other bar under linear.
// Hovering shows a HealthStrip-style fixed-position readout; clicking
// selects the slice (parent-owned, time-addressed) to filter the groups
// card below — no pin machinery, so none of HealthStrip's dismissal dance.
export default function IncidentTimeline({
  events,
  win,
  nowMs,
  selected,
  onSelect,
}: {
  events: OutageEvent[]
  win: Window
  nowMs: number
  selected: number | null
  onSelect: (bucketStartMs: number | null) => void
}) {
  const { mode } = useTimezone()
  const utc = mode === 'utc'
  const [hover, setHover] = useState<{ i: number; x: number; y: number; below: boolean } | null>(null)
  const grid = timelineGrid(win, nowMs)
  const { startMs, endMs, bucketMs } = grid
  const withZone = gridWithZone(grid, win, utc)
  const withYear = gridWithYear(grid)

  // Memoized on the data inputs: open events are uncapped, so a mesh-wide
  // outage can put thousands of events through this events × buckets pass,
  // and hover re-renders must not repeat it mid-incident.
  const { slots, maxTotal, peak } = useMemo(() => {
    const built: Slot[] = []
    for (let i = 0; i < GRID_SLOTS; i++)
      built.push({ t: startMs + i * bucketMs, openOffline: 0, openProbe: 0, resOffline: 0, resProbe: 0 })
    for (const e of events) {
      // Same future-skew clamps as overlapsBucket — the two must agree.
      const closed = Math.min(e.closed_at ? Date.parse(e.closed_at) : nowMs, nowMs)
      const opened = Math.min(Date.parse(e.opened_at), closed)
      // Integer slot range of the clamped lifetime; the bounds reproduce
      // overlapsBucket exactly (half-open: ceil−1 lands one bucket earlier
      // than floor when closed sits exactly on a boundary, matching its
      // strict > comparison).
      const lo = Math.max(0, Math.floor((opened - startMs) / bucketMs))
      const hi = Math.min(GRID_SLOTS - 1, Math.ceil((closed - startMs) / bucketMs) - 1)
      for (let i = lo; i <= hi; i++) {
        const s = built[i]
        if (e.closed_at == null) {
          if (e.kind === 'agent_offline') s.openOffline++
          else s.openProbe++
        } else if (e.kind === 'agent_offline') s.resOffline++
        else s.resProbe++
      }
    }
    const busiest = Math.max(...built.map(slotTotal))
    return { slots: built, maxTotal: Math.max(1, busiest), peak: busiest }
  }, [events, startMs, bucketMs, nowMs])

  const slotAt = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect()
    const i = Math.min(GRID_SLOTS - 1, Math.max(0, Math.floor(((e.clientX - r.left) / r.width) * GRID_SLOTS)))
    // Clamp so the centered card stays on screen at the viewport edges;
    // flip it below the chart when a scrolled page leaves no headroom
    // above. The threshold must cover the card's tallest content (wrapped
    // range + pill + captions), not HealthStrip's shorter 150.
    const x = Math.min(Math.max(r.left + ((i + 0.5) / GRID_SLOTS) * r.width, 140), window.innerWidth - 140)
    const below = r.top < 260
    const y = below ? r.bottom : r.top
    return { i, x, y, below }
  }

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const p = slotAt(e)
    setHover((prev) =>
      prev && prev.i === p.i && prev.x === p.x && prev.y === p.y && prev.below === p.below ? prev : p,
    )
  }

  // Clicking toggles the slice filter; empty slices are selectable too —
  // showing the honest empty state beats a dead click.
  const onClick = (e: React.MouseEvent<SVGSVGElement>) => {
    const t = slots[slotAt(e).i].t
    onSelect(t === selected ? null : t)
  }

  const ariaLabel =
    events.length === 0
      ? `Incident timeline, last ${win}: no incidents`
      : `Incident timeline, last ${win}: ${events.length} incidents, busiest slice ${peak}`

  const slot = hover ? slots[hover.i] : null
  const tip = slot
    ? {
        open: slot.openOffline + slot.openProbe,
        res: slot.resOffline + slot.resProbe,
        offline: slot.openOffline + slot.resOffline,
        probe: slot.openProbe + slot.resProbe,
      }
    : null
  return (
    <>
      {/* Pointer-gained readout mirroring the aggregate aria-label, as on
          the fleet health strips; per-slot focus targets are the eventual
          fix for keyboard selection (the clear-filter chip in the groups
          card keeps an applied filter keyboard-recoverable meanwhile). */}
      {/* oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/click-events-have-key-events */}
      <svg
        className="incident-timeline"
        viewBox={`0 0 ${GRID_SLOTS * SLOT_W} ${CHART_H}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={ariaLabel}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
        onClick={onClick}
      >
        {slots.map((s) => {
          const i = (s.t - startMs) / bucketMs
          const x = i * SLOT_W + (SLOT_W - BAR_W) / 2
          const open = s.openOffline + s.openProbe
          const res = s.resOffline + s.resProbe
          const total = open + res
          const isSelected = selected === s.t
          const backdrop = isSelected && (
            <rect className="itl-slot-selected" x={i * SLOT_W} y={0} width={SLOT_W} height={CHART_H} />
          )
          if (total === 0)
            return (
              <g key={s.t}>
                {backdrop}
                <rect className="itl-bar-zero" x={x} y={BASELINE - 1} width={BAR_W} height={1} />
              </g>
            )
          const h = (MAX_BAR * Math.sqrt(total)) / Math.sqrt(maxTotal)
          // Stack by kind — probe failures (warn) at the baseline, agent
          // outages (crit) above — each kind solid (open) under muted
          // (resolved), so a wide bucket holding both kinds still shows its
          // mix instead of only the worst. Proportional split, floored so a
          // small segment stays visible; the 1-unit surface gap separates
          // the kind blocks only (the solid→muted edge already marks the
          // state boundary within a kind).
          const segs = [
            { n: s.openProbe, kind: 'warn', cls: 'itl-bar-open sev-warn' },
            { n: s.resProbe, kind: 'warn', cls: 'itl-bar-resolved sev-warn' },
            { n: s.openOffline, kind: 'crit', cls: 'itl-bar-open sev-crit' },
            { n: s.resOffline, kind: 'crit', cls: 'itl-bar-resolved sev-crit' },
          ].filter((seg) => seg.n > 0)
          const selCls = isSelected ? ' itl-bar-selected' : ''
          const rects = []
          let y = BASELINE
          for (let j = 0; j < segs.length; j++) {
            if (j > 0 && segs[j].kind !== segs[j - 1].kind) y -= 1
            const segH = Math.max((h * segs[j].n) / total, 1.5)
            y -= segH
            rects.push(
              <rect
                key={segs[j].cls}
                className={segs[j].cls + selCls}
                x={x}
                y={y}
                width={BAR_W}
                height={segH}
                rx={1}
              />,
            )
          }
          return (
            <g key={s.t}>
              {backdrop}
              {rects}
            </g>
          )
        })}
      </svg>
      <div className="incident-timeline-axis" aria-hidden="true">
        <span>{axisLabel(startMs, win, utc, withZone, withYear)}</span>
        {/* The flexbox centers the middle span, so its text must be the
            true midpoint — a mid-bucket time now that the slot count is
            odd. */}
        <span>{axisLabel((startMs + endMs) / 2, win, utc, withZone, withYear)}</span>
        <span>
          {axisLabel(endMs, win, utc, withZone, withYear)}
          {utc ? ' UTC' : ''}
        </span>
      </div>
      {hover && slot && tip && (
        <div
          className={'map-tip strip-tip incident-tip' + (hover.below ? ' strip-tip-below' : '')}
          role="status"
          style={{ left: hover.x, top: hover.y }}
        >
          <div className="map-tip-head">
            <b>{bucketRangeLabel(slot.t, bucketMs, win, utc, withZone, withYear)}</b>
            {/* The category labels are the incident card's own (it applies
                them to resolved groups too); the open/resolved caption
                carries the state, so the pill must not imply "ongoing". */}
            {tip.offline + tip.probe > 0 && (
              <span className={`map-tip-pill sev-${tip.offline > 0 ? 'crit' : 'warn'}`}>
                {tip.offline > 0 ? 'Agents offline' : 'Probe failures'}
              </span>
            )}
          </div>
          <div className="map-tip-value">
            {tip.open + tip.res === 0 ? '—' : tip.open + tip.res}
            <small> {tip.open + tip.res === 1 ? 'incident' : 'incidents'}</small>
          </div>
          <div className="map-tip-caption">
            {tip.open + tip.res === 0
              ? 'no incidents in this slice'
              : `${tip.open} open · ${tip.res} resolved — ${tip.offline} agent outage${tip.offline === 1 ? '' : 's'} · ${tip.probe} probe failure${tip.probe === 1 ? '' : 's'}`}
          </div>
          {selected === slot.t && <div className="map-tip-caption">Filtering groups below — click again to clear</div>}
        </div>
      )}
    </>
  )
}
