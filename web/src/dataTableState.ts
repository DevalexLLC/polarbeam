export type DataTableOrder = 'asc' | 'desc'

export interface DataTableSort {
  key: string
  order: DataTableOrder
}

export interface DataTablePage {
  limit: number
  offset: number
  total: number
  has_more: boolean
}

export function nextDataTableSort(current: DataTableSort, key: string): DataTableSort {
  if (current.key !== key) return { key, order: 'asc' }
  return { key, order: current.order === 'asc' ? 'desc' : 'asc' }
}

export function dataTablePageNumber(page: DataTablePage): number {
  return Math.floor(page.offset / page.limit) + 1
}

export function dataTablePageCount(page: DataTablePage): number {
  return Math.max(1, Math.ceil(page.total / page.limit))
}

export function dataTableResultRange(page: DataTablePage, rowCount: number): string {
  if (page.total === 0 || rowCount === 0) return '0 results'
  const first = page.offset + 1
  const last = page.offset + rowCount
  return `Showing ${first}–${last} of ${page.total}`
}

export function dataTableMissingKeys(
  rowKeys: readonly string[],
  expandedKey: string | null,
  actionKey: string | null,
): { expandedMissing: boolean; actionMissing: boolean } {
  const keys = new Set(rowKeys)
  return {
    expandedMissing: expandedKey !== null && !keys.has(expandedKey),
    actionMissing: actionKey !== null && !keys.has(actionKey),
  }
}
