import { Fragment, useEffect, useLayoutEffect, useMemo, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import DisclosureChevron from './DisclosureChevron'
import {
  dataTableMissingKeys,
  dataTablePageCount,
  dataTablePageNumber,
  dataTableResultRange,
  nextDataTableSort,
  type DataTablePage,
  type DataTableSort,
} from '../dataTableState'

export type DataTablePriority = 'identity' | 'status' | 'primary' | 'secondary'

export interface DataTableColumn<Row> {
  key: string
  label: string
  render: (row: Row) => ReactNode
  sortKey?: string
  priority?: DataTablePriority
  className?: string
}

export interface DataTableDisclosure<Row> {
  expandedKey: string | null
  onExpandedKeyChange: (key: string | null) => void
  label: (row: Row, expanded: boolean) => string
  render?: (row: Row, surface: 'desktop' | 'mobile') => ReactNode
  desktop?: boolean
}

function FloatingActionMenu({
  id,
  label,
  trigger,
  onClose,
  children,
}: {
  id: string
  label: string
  trigger: HTMLButtonElement
  onClose: () => void
  children: ReactNode
}) {
  const menu = useRef<HTMLDivElement>(null)

  useLayoutEffect(() => {
    const place = () => {
      const element = menu.current
      if (!element) return
      const anchor = trigger.getBoundingClientRect()
      const gap = 4
      const top =
        anchor.bottom + gap + element.offsetHeight <= window.innerHeight
          ? anchor.bottom + gap
          : Math.max(gap, anchor.top - gap - element.offsetHeight)
      const left = Math.min(
        Math.max(gap, anchor.right - element.offsetWidth),
        Math.max(gap, window.innerWidth - element.offsetWidth - gap),
      )
      element.style.top = `${top}px`
      element.style.left = `${left}px`
      element.style.visibility = 'visible'
    }
    place()
    menu.current?.querySelector<HTMLElement>('button, a, input, select, [tabindex]:not([tabindex="-1"])')?.focus()
    window.addEventListener('resize', place)
    window.addEventListener('scroll', place, true)
    return () => {
      window.removeEventListener('resize', place)
      window.removeEventListener('scroll', place, true)
    }
  }, [trigger])

  return createPortal(
    <div
      ref={menu}
      id={id}
      className="data-table-actions-menu"
      role="toolbar"
      tabIndex={-1}
      data-action-key={id}
      aria-label={label}
      style={{ top: 0, left: 0, visibility: 'hidden' }}
      onKeyDown={(event) => {
        if (event.key !== 'Escape') return
        onClose()
        trigger.focus()
      }}
    >
      {children}
    </div>,
    document.body,
  )
}

export interface DataTableActions<Row> {
  openKey: string | null
  onOpenKeyChange: (key: string | null) => void
  label: (row: Row) => string
  render: (row: Row) => ReactNode
}

export interface DataTableProps<Row> {
  label: string
  rows: readonly Row[]
  rowKey: (row: Row) => string
  columns: readonly DataTableColumn<Row>[]
  sort?: DataTableSort
  onSortChange?: (sort: DataTableSort) => void
  page?: DataTablePage
  onPageChange?: (page: number) => void
  resultLabel?: string
  loading?: boolean
  error?: ReactNode
  emptyTitle: string
  emptyDescription: string
  disclosure?: DataTableDisclosure<Row>
  actions?: DataTableActions<Row>
  rowClassName?: (row: Row) => string
  rowID?: (row: Row) => string
}

function statePanel(className: string, role: 'status' | 'alert', content: ReactNode) {
  return (
    <div className={className} role={role}>
      {content}
    </div>
  )
}

export default function DataTable<Row>({
  label,
  rows,
  rowKey,
  columns,
  sort,
  onSortChange,
  page,
  onPageChange,
  resultLabel = 'results',
  loading = false,
  error,
  emptyTitle,
  emptyDescription,
  disclosure,
  actions,
  rowClassName,
  rowID,
}: DataTableProps<Row>) {
  const root = useRef<HTMLDivElement>(null)
  const focusedRow = useRef<string | null>(null)
  const actionTrigger = useRef<HTMLButtonElement | null>(null)
  const keys = useMemo(() => rows.map(rowKey), [rowKey, rows])
  const { expandedMissing, actionMissing } = dataTableMissingKeys(
    keys,
    disclosure?.expandedKey ?? null,
    actions?.openKey ?? null,
  )

  useEffect(() => {
    if (expandedMissing) disclosure?.onExpandedKeyChange(null)
    if (actionMissing) actions?.onOpenKeyChange(null)
    if (focusedRow.current !== null && !keys.includes(focusedRow.current)) {
      focusedRow.current = null
      root.current?.focus()
    }
  }, [actionMissing, actions, disclosure, expandedMissing, keys])

  let initialState: ReactNode = null
  if (loading && rows.length === 0) {
    initialState = statePanel(
      'data-table-state state-panel',
      'status',
      <>
        <span className="state-spinner" /> Loading {label.toLowerCase()}…
      </>,
    )
  } else if (error && rows.length === 0) {
    initialState = statePanel('data-table-state inline-alert', 'alert', error)
  } else if (rows.length === 0) {
    initialState = (
      <div className="data-table-state empty-state">
        <strong>{emptyTitle}</strong>
        <span>{emptyDescription}</span>
      </div>
    )
  }

  const renderSortHeader = (column: DataTableColumn<Row>) => {
    const active = sort?.key === column.sortKey
    const ariaSort = active ? (sort?.order === 'asc' ? 'ascending' : 'descending') : undefined
    return (
      <th key={column.key} scope="col" aria-sort={ariaSort} className={column.className}>
        {column.sortKey && sort && onSortChange ? (
          <button
            type="button"
            className="data-table-sort"
            onClick={() => onSortChange(nextDataTableSort(sort!, column.sortKey!))}
          >
            {column.label}
            <span aria-hidden="true" className="data-table-sort-mark">
              {active ? (sort.order === 'asc' ? '↑' : '↓') : '↕'}
            </span>
          </button>
        ) : (
          column.label
        )}
      </th>
    )
  }

  const renderActions = (row: Row, key: string) => {
    if (!actions) return null
    const open = actions.openKey === key
    const menuID = `data-table-actions-${key}`
    return (
      <div className="data-table-actions">
        <button
          type="button"
          className="secondary-button data-table-actions-toggle"
          aria-label={actions.label(row)}
          aria-expanded={open}
          aria-controls={menuID}
          onClick={(event) => {
            actionTrigger.current = open ? null : event.currentTarget
            actions.onOpenKeyChange(open ? null : key)
          }}
        >
          Actions
        </button>
      </div>
    )
  }

  const renderDisclosure = (row: Row, key: string, surface: 'desktop' | 'mobile') => {
    if (!disclosure) return null
    const expanded = disclosure.expandedKey === key
    const detailsID = `data-table-detail-${surface}-${key}`
    return (
      <button
        type="button"
        className="data-table-disclosure"
        aria-expanded={expanded}
        aria-controls={detailsID}
        onClick={() => disclosure.onExpandedKeyChange(expanded ? null : key)}
      >
        <span>{disclosure.label(row, expanded)}</span>
        <DisclosureChevron expanded={expanded} />
      </button>
    )
  }

  const pageNumber = page ? dataTablePageNumber(page) : 1
  const pageCount = page ? dataTablePageCount(page) : 1
  const sortColumn = columns.find((column) => column.sortKey === sort?.key)
  const openActionRow = actions?.openKey ? rows.find((row) => rowKey(row) === actions.openKey) : undefined

  return (
    // Focus capture records the stable row identity so a refresh that
    // removes that row can return focus to this table region.
    // oxlint-disable-next-line jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/no-static-element-interactions
    <div
      ref={root}
      className="data-table-root"
      role="region"
      aria-label={`${label} data table`}
      tabIndex={-1}
      onFocusCapture={(event) => {
        focusedRow.current = (event.target as Element).closest<HTMLElement>('[data-row-key]')?.dataset.rowKey ?? null
      }}
    >
      {initialState ?? (
        <>
          {sort && sortColumn && (
            <span className="sr-only" aria-live="polite">
              Sorted by {sortColumn.label}, {sort.order === 'asc' ? 'ascending' : 'descending'}
            </span>
          )}
          {error && rows.length > 0 && <div className="inline-alert data-table-refresh-error">{error}</div>}
          <div className="data-table-desktop">
            <table className="data-table" aria-label={label}>
              <thead>
                <tr>
                  {columns.map(renderSortHeader)}
                  {disclosure?.render && disclosure.desktop !== false && (
                    <th scope="col">
                      <span className="sr-only">Details</span>
                    </th>
                  )}
                  {actions && (
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  )}
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => {
                  const key = rowKey(row)
                  const expanded = disclosure?.expandedKey === key
                  const desktopDisclosure = disclosure?.render && disclosure.desktop !== false
                  return (
                    <Fragment key={key}>
                      <tr
                        id={rowID ? rowID(row) + '-desktop' : undefined}
                        data-row-key={key}
                        className={rowClassName?.(row)}
                      >
                        {columns.map((column) => (
                          <td key={column.key} className={column.className}>
                            {column.render(row)}
                          </td>
                        ))}
                        {desktopDisclosure && disclosure && (
                          <td className="data-table-disclosure-cell">
                            <button
                              type="button"
                              className="data-table-disclosure"
                              aria-expanded={expanded}
                              aria-controls={`data-table-detail-desktop-${key}`}
                              onClick={() => disclosure.onExpandedKeyChange(expanded ? null : key)}
                            >
                              <span>{disclosure.label(row, expanded)}</span>
                              <DisclosureChevron expanded={expanded} />
                            </button>
                          </td>
                        )}
                        {actions && <td className="data-table-actions-cell">{renderActions(row, key)}</td>}
                      </tr>
                      {expanded && desktopDisclosure && disclosure?.render && (
                        <tr className="data-table-detail-row">
                          <td
                            id={`data-table-detail-desktop-${key}`}
                            colSpan={columns.length + (desktopDisclosure ? 1 : 0) + (actions ? 1 : 0)}
                          >
                            {disclosure.render(row, 'desktop')}
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </table>
          </div>
          <div className="data-table-mobile" role="list" aria-label={label}>
            {rows.map((row) => {
              const key = rowKey(row)
              const primary = columns.filter((column) => column.priority !== 'secondary')
              const secondary = columns.filter((column) => column.priority === 'secondary')
              return (
                <article
                  key={key}
                  id={rowID ? rowID(row) + '-mobile' : undefined}
                  role="listitem"
                  data-row-key={key}
                  className={`data-table-mobile-row ${rowClassName?.(row) ?? ''}`}
                >
                  <div className="data-table-mobile-primary">
                    {primary.map((column) => (
                      <div
                        key={column.key}
                        className={`data-table-mobile-field data-table-${column.priority ?? 'primary'}`}
                      >
                        <span className="data-table-mobile-label">{column.label}</span>
                        <div className={column.className}>{column.render(row)}</div>
                      </div>
                    ))}
                  </div>
                  {(secondary.length > 0 || disclosure?.render) && renderDisclosure(row, key, 'mobile')}
                  {disclosure?.expandedKey === key && secondary.length > 0 && (
                    <div id={`data-table-detail-mobile-${key}`} className="data-table-mobile-secondary">
                      {secondary.map((column) => (
                        <div key={column.key} className="data-table-mobile-field">
                          <span className="data-table-mobile-label">{column.label}</span>
                          <div className={column.className}>{column.render(row)}</div>
                        </div>
                      ))}
                    </div>
                  )}
                  {disclosure?.expandedKey === key && disclosure.render && (
                    <div
                      id={secondary.length === 0 ? `data-table-detail-mobile-${key}` : undefined}
                      className="data-table-detail"
                    >
                      {disclosure.render(row, 'mobile')}
                    </div>
                  )}
                  {actions && renderActions(row, key)}
                </article>
              )
            })}
          </div>
          {page && onPageChange && (
            <div className="data-table-pager">
              <span className="hint">
                {dataTableResultRange(page, rows.length)} {resultLabel} · Page {pageNumber} of {pageCount}
              </span>
              <span className="data-table-pager-actions">
                <button
                  type="button"
                  className="secondary-button"
                  disabled={pageNumber === 1}
                  onClick={() => onPageChange(pageNumber - 1)}
                >
                  Previous
                </button>
                <button
                  type="button"
                  className="secondary-button"
                  disabled={!page.has_more}
                  onClick={() => onPageChange(pageNumber + 1)}
                >
                  Next
                </button>
              </span>
            </div>
          )}
          {actions && openActionRow && actionTrigger.current && (
            <FloatingActionMenu
              id={`data-table-actions-${actions.openKey}`}
              label={actions.label(openActionRow)}
              trigger={actionTrigger.current}
              onClose={() => {
                actions.onOpenKeyChange(null)
                actionTrigger.current = null
              }}
            >
              {actions.render(openActionRow)}
            </FloatingActionMenu>
          )}
        </>
      )}
    </div>
  )
}
