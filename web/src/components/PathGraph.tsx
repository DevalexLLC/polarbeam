import { useMemo } from 'react'
import { fmtLatency } from '../format'
import type { Hop } from '../types'

// PathGraph draws traceroute paths as a layered left-to-right DAG: one
// column per TTL (consecutive all-silent TTLs collapse into a single
// "no reply" column), one node per distinct responder address, edges from
// each path's TTL-adjacency. Adjacency is inferred — traceroute has no
// flow-level pairing — so latency is shown per node (the hop's best RTT),
// never per edge, where deltas between unrelated probe flows would lie.
//
// 'current' mode merges any number of paths (edge width = how many share
// the link); 'diff' mode takes exactly two (old, new) and colors elements
// only in the old path as removed, only in the new as added.

export interface GraphPath {
  key: string
  hops: Hop[]
  destReached: boolean
}

interface Props {
  mode: 'current' | 'diff'
  source: string
  dest: string
  paths: GraphPath[]
}

interface GNode {
  id: string
  col: number // hop column index, 0-based (terminals are separate)
  label: string
  full: string // hover title (untruncated addr)
  silent: boolean
  // rtts is only fed when this node was the hop's sole responder — the
  // wire format doesn't pair rtt_us entries to addrs, so a multi-responder
  // hop's timings can't be attributed to one address. hopRtts always
  // collects the hop-level minimum for the hover title.
  rtts: number[]
  hopRtts: number[]
  members: Set<string>
}

interface GEdge {
  from: string
  to: string
  members: Set<string>
  unreached: boolean // final link of a path that never reached dest
}

interface Column {
  silent: boolean
  ttlCount: number
  nodes: GNode[]
}

const SRC = '__src'
const DST = '__dst'
const MAX_LABEL = 22

function shortAddr(addr: string): string {
  if (addr.length <= MAX_LABEL) return addr
  return addr.slice(0, 11) + '…' + addr.slice(-10)
}

function buildGraph(paths: GraphPath[]) {
  const byTTL = paths.map((p) => {
    const m = new Map<number, Hop>()
    for (const h of p.hops) m.set(h.ttl, h)
    return m
  })
  const last = paths.map((p) => Math.max(0, ...p.hops.map((h) => h.ttl)))
  const maxTTL = Math.max(0, ...last)

  // A TTL layer is silent when no path that spans it heard a responder.
  const silentTTL: boolean[] = []
  for (let t = 1; t <= maxTTL; t++) {
    silentTTL[t] = !paths.some((_, i) => t <= last[i] && (byTTL[i].get(t)?.addrs.length ?? 0) > 0)
  }

  // Collapse runs of silent layers into single columns.
  const colOfTTL: number[] = []
  const columns: Column[] = []
  for (let t = 1; t <= maxTTL; t++) {
    const prev = columns[columns.length - 1]
    if (silentTTL[t] && prev && prev.silent && colOfTTL[t - 1] === columns.length - 1) {
      prev.ttlCount++
      colOfTTL[t] = columns.length - 1
    } else {
      colOfTTL[t] = columns.length
      columns.push({ silent: silentTTL[t], ttlCount: 1, nodes: [] })
    }
  }

  const nodes = new Map<string, GNode>()
  const edges = new Map<string, GEdge>()
  const node = (col: number, addr: string | null): GNode => {
    const id = col + ':' + (addr ?? '*')
    let n = nodes.get(id)
    if (!n) {
      n = {
        id,
        col,
        label: addr ? shortAddr(addr) : '*',
        full: addr ?? 'no responder',
        silent: addr === null,
        rtts: [],
        hopRtts: [],
        members: new Set(),
      }
      nodes.set(id, n)
      columns[col].nodes.push(n)
    }
    return n
  }
  const edge = (from: string, to: string, key: string, unreached: boolean) => {
    if (from === to) return
    const id = from + '>' + to
    let e = edges.get(id)
    if (!e) {
      e = { from, to, members: new Set(), unreached }
      edges.set(id, e)
    }
    e.members.add(key)
    if (!unreached) e.unreached = false
  }

  const srcMembers = new Set<string>()
  const dstMembers = new Set<string>()
  paths.forEach((p, i) => {
    let prev = [SRC]
    srcMembers.add(p.key)
    for (let t = 1; t <= last[i]; t++) {
      const col = colOfTTL[t]
      const hop = byTTL[i].get(t)
      const addrs = hop?.addrs ?? []
      const cur: GNode[] = addrs.length === 0 ? [node(col, null)] : addrs.map((a) => node(col, a))
      const best = hop && hop.rtt_us.length > 0 ? Math.min(...hop.rtt_us) : null
      for (const n of cur) {
        n.members.add(p.key)
        if (best !== null && !n.silent) {
          n.hopRtts.push(best)
          if (addrs.length === 1) n.rtts.push(best)
        }
      }
      for (const f of prev) for (const n of cur) edge(f, n.id, p.key, false)
      if (cur.length > 0) prev = cur.map((n) => n.id)
    }
    dstMembers.add(p.key)
    for (const f of prev) edge(f, DST, p.key, !p.destReached)
  })

  return { columns, nodes, edges, srcMembers, dstMembers }
}

// Geometry constants: 11px mono labels, two-line nodes.
const CHAR_W = 6.7
const PAD_X = 8
const NODE_H = 34
const TERM_H = 38
const COL_GAP = 42
const ROW_GAP = 12
const MARGIN = 4

function nodeWidth(label: string): number {
  return Math.ceil(label.length * CHAR_W) + PAD_X * 2
}

type DiffState = 'same' | 'added' | 'removed'

function diffState(members: Set<string>, oldKey: string): DiffState {
  const hasOld = members.has(oldKey)
  return hasOld && members.size > 1 ? 'same' : hasOld ? 'removed' : 'added'
}

export default function PathGraph({ mode, source, dest, paths }: Props) {
  const g = useMemo(() => buildGraph(paths), [paths])
  if (paths.length === 0) return null
  const oldKey = paths[0].key // diff mode: first path is the old one

  // Column widths: terminals, then one width per hop column.
  const srcW = nodeWidth(source) + 8
  const reached = paths.filter((p) => p.destReached).length
  const destSub =
    mode === 'diff'
      ? ''
      : reached === paths.length
        ? 'destination reached'
        : reached === 0
          ? 'not reached'
          : `reached ${reached}/${paths.length}`
  const dstW = Math.max(nodeWidth(dest), nodeWidth(destSub)) + 8
  const colW = g.columns.map((c) =>
    Math.max(60, ...c.nodes.map((n) => nodeWidth(n.silent ? `no reply ×${c.ttlCount}` : n.label))),
  )

  const colX: number[] = []
  let x = MARGIN + srcW + COL_GAP
  for (let c = 0; c < g.columns.length; c++) {
    colX[c] = x
    x += colW[c] + COL_GAP
  }
  const dstX = x
  const width = dstX + dstW + MARGIN

  // The accessible hop count is TTLs, not rendered columns — a collapsed
  // silent run still represents its full span of hops.
  const hopCount = g.columns.reduce((acc, c) => acc + c.ttlCount, 0)

  const stackH = (n: number) => n * NODE_H + (n - 1) * ROW_GAP
  const maxStack = Math.max(TERM_H, ...g.columns.map((c) => stackH(c.nodes.length)))
  const height = maxStack + MARGIN * 2
  const midY = height / 2

  // Node centers.
  const cx = new Map<string, number>()
  const cy = new Map<string, number>()
  const w = new Map<string, number>()
  cx.set(SRC, MARGIN + srcW / 2)
  cy.set(SRC, midY)
  w.set(SRC, srcW)
  cx.set(DST, dstX + dstW / 2)
  cy.set(DST, midY)
  w.set(DST, dstW)
  g.columns.forEach((c, i) => {
    const top = midY - stackH(c.nodes.length) / 2
    c.nodes.forEach((n, j) => {
      cx.set(n.id, colX[i] + colW[i] / 2)
      cy.set(n.id, top + j * (NODE_H + ROW_GAP) + NODE_H / 2)
      w.set(n.id, colW[i])
    })
  })

  const edgePath = (e: GEdge): string => {
    const x1 = (cx.get(e.from) ?? 0) + (w.get(e.from) ?? 0) / 2
    const y1 = cy.get(e.from) ?? 0
    const x2 = (cx.get(e.to) ?? 0) - (w.get(e.to) ?? 0) / 2
    const y2 = cy.get(e.to) ?? 0
    const dx = Math.max(14, (x2 - x1) / 2)
    return `M${x1} ${y1} C${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`
  }

  const edgeClass = (e: GEdge): string => {
    if (e.unreached) return 'pg-edge pg-unreached'
    if (mode === 'diff') return 'pg-edge pg-' + diffState(e.members, oldKey)
    return 'pg-edge'
  }
  const nodeClass = (n: GNode): string => {
    const base = n.silent ? 'pg-node pg-silent' : 'pg-node'
    if (mode === 'diff') return base + ' pg-' + diffState(n.members, oldKey)
    return base
  }

  return (
    <div className="scroll-x path-graph">
      {mode === 'diff' && (
        <div className="pg-legend" aria-hidden="true">
          <span className="pg-key pg-key-removed">old path</span>
          <span className="pg-key pg-key-added">new path</span>
        </div>
      )}
      <svg
        width={width}
        height={height}
        role="img"
        aria-label={`Traceroute path graph from ${source} to ${dest}: ${hopCount} ${hopCount === 1 ? 'hop' : 'hops'}`}
      >
        {[...g.edges.values()].map((e) => (
          <path
            key={e.from + '>' + e.to}
            className={edgeClass(e)}
            d={edgePath(e)}
            strokeWidth={mode === 'current' ? Math.min(4, 1.4 + 0.9 * (e.members.size - 1)) : 1.6}
          />
        ))}
        <g className="pg-term">
          <rect x={MARGIN} y={midY - TERM_H / 2} width={srcW} height={TERM_H} rx="8" />
          <text x={MARGIN + srcW / 2} y={midY} className="pg-label">
            {source}
          </text>
        </g>
        {g.columns.map((c, i) =>
          c.nodes.map((n) => {
            const nx = cx.get(n.id) ?? 0
            const ny = cy.get(n.id) ?? 0
            const best = n.rtts.length > 0 ? Math.min(...n.rtts) : null
            const hopBest = n.hopRtts.length > 0 ? Math.min(...n.hopRtts) : null
            return (
              <g key={n.id} className={nodeClass(n)}>
                <title>
                  {n.silent
                    ? `no responder for ${c.ttlCount} ${c.ttlCount === 1 ? 'hop' : 'hops'}`
                    : n.full +
                      (best !== null
                        ? ` · ${fmtLatency(best)}`
                        : hopBest !== null
                          ? ` · hop min ${fmtLatency(hopBest)} (shared across responders)`
                          : '')}
                </title>
                <rect x={nx - colW[i] / 2} y={ny - NODE_H / 2} width={colW[i]} height={NODE_H} rx="6" />
                {n.silent ? (
                  <text x={nx} y={ny} className="pg-label">
                    no reply {c.ttlCount > 1 ? `×${c.ttlCount}` : ''}
                  </text>
                ) : (
                  <>
                    <text x={nx} y={ny - 6} className="pg-label">
                      {n.label}
                    </text>
                    <text x={nx} y={ny + 8} className="pg-sub">
                      {best !== null ? fmtLatency(best) : '—'}
                    </text>
                  </>
                )}
              </g>
            )
          }),
        )}
        <g
          className={
            'pg-term pg-dest ' + (mode === 'diff' ? '' : reached === paths.length ? 'pg-dest-ok' : 'pg-dest-warn')
          }
        >
          <rect x={dstX} y={midY - TERM_H / 2} width={dstW} height={TERM_H} rx="8" />
          {destSub === '' ? (
            <text x={dstX + dstW / 2} y={midY} className="pg-label">
              {dest}
            </text>
          ) : (
            <>
              <text x={dstX + dstW / 2} y={midY - 6} className="pg-label">
                {dest}
              </text>
              <text x={dstX + dstW / 2} y={midY + 8} className="pg-sub">
                {destSub}
              </text>
            </>
          )}
        </g>
      </svg>
    </div>
  )
}
