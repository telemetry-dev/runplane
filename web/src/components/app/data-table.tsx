import { createLink } from '@tanstack/react-router'
import { useCallback, useState, type AnchorHTMLAttributes, type CSSProperties, type ReactNode, type Ref } from 'react'
import { cx } from '~/lib/cx'

// The bordered tables of the designs (Running, Recent jobs, Jobs, Servers). The
// columns are defined once on the table, and every row uses the same grid, so
// cells line up across rows whatever their content:
//
//   <DataTable aria-label="Running jobs" columns={[16, 'grow', 200, 150, 120, 96, 56]}>
//     <DataTableHeader>
//       <DataTableHead />
//       <DataTableHead>Job</DataTableHead>
//       …
//       <DataTableHead align="end">Elapsed</DataTableHead>
//     </DataTableHeader>
//     {jobs.map((job) => (
//       <DataTableRow key={job.id}>
//         <DataTableCell><StatusIcon status="running" /></DataTableCell>
//         <DataTableCell><RowLink to="/jobs/$jobId" params={{ jobId: job.id }}>{job.name}</RowLink></DataTableCell>
//         …
//       </DataTableRow>
//     ))}
//   </DataTable>
//
// A RowLink makes the whole row a link and gives it the hover fill. Buttons in a
// linked row need `className="relative"` on their cell to stay above the link.
//
// A table too narrow for its columns drops the less useful ones (useFittingColumns),
// and then narrows its growing column before it scrolls sideways.

export type DataTableColumn = number | 'grow'

type DataTableProps = {
  /** One entry per column: a width in pixels, or 'grow' for a column that takes the rest. */
  columns: readonly DataTableColumn[]
  /** Vertical padding of the body rows: xs for dense lists, s for jobs (default), m for servers. */
  rowSpacing?: 'xs' | 's' | 'm'
  'aria-label': string
  className?: string
  children: ReactNode
}

export function DataTable({ columns, rowSpacing = 's', className, children, ...props }: DataTableProps) {
  const style = {
    '--table-columns': columns.map((column) => (column === 'grow' ? 'minmax(0,1fr)' : `${column}px`)).join(' '),
    '--row-spacing': `var(--space-${rowSpacing})`,
  } as CSSProperties
  return (
    <div
      role="table"
      className={cx(
        // The 1px edge is an overlay, so row fills do not hide it; the padding gives it
        // its own pixel, as a border would, so tables are as tall as in the designs.
        'relative overflow-hidden rounded-m bg-neutral-1 p-px',
        'after:pointer-events-none after:absolute after:inset-zero after:rounded-[inherit] after:shadow-[inset_0_0_0_1px_var(--neutral-4)]',
        className,
      )}
      style={style}
      {...props}
    >
      <div className="overflow-x-auto">
        <div style={{ minWidth: minWidth(columns) }}>{children}</div>
      </div>
    </div>
  )
}

// The divider under each row gets its own pixel of padding, as a border would.
const row = cx(
  'grid grid-cols-(--table-columns) items-center gap-m px-m py-(--row-spacing)',
  '[&:not(:last-child)]:pb-[calc(var(--row-spacing)+1px)] [&:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]',
)

export function DataTableHeader({ children }: { children: ReactNode }) {
  return (
    <div role="row" className={cx(row, '[--row-spacing:var(--space-xs)]')}>
      {children}
    </div>
  )
}

type Align = 'start' | 'end'

export function DataTableHead({ align = 'start', children }: { align?: Align; children?: ReactNode }) {
  return (
    <div role="columnheader" className={cx('min-w-0 truncate text-xs font-medium text-neutral-7', align === 'end' && 'text-right')}>
      {children}
    </div>
  )
}

type DataTableRowProps = {
  /** The row of the open item, for example the selected step. */
  selected?: boolean
  className?: string
  children: ReactNode
}

export function DataTableRow({ selected = false, className, children }: DataTableRowProps) {
  return (
    <div
      role="row"
      aria-selected={selected || undefined}
      className={cx(
        row,
        'relative transition-[background-color] has-[[data-row-link]]:hover:bg-neutral-2 aria-selected:bg-neutral-2',
        className,
      )}
    >
      {children}
    </div>
  )
}

type DataTableCellProps = {
  align?: Align
  className?: string
  children?: ReactNode
}

export function DataTableCell({ align = 'start', className, children }: DataTableCellProps) {
  return (
    <div
      role="cell"
      className={cx('flex min-w-0 items-center gap-xs text-s text-neutral-8', align === 'end' && 'justify-end text-right', className)}
    >
      {children}
    </div>
  )
}

/** A row that spans all columns, for "No jobs match these filters." */
export function DataTableEmpty({ children }: { children: ReactNode }) {
  return (
    <div role="row" className="px-m py-xl">
      <div role="cell" className="text-center text-s text-neutral-7">
        {children}
      </div>
    </div>
  )
}

/**
 * The head of a 16px icon column, with `label` ("Status") for screen readers.
 * Its 16px slot makes the header 2px taller, as in the Servers design.
 */
export function DataTableIconHead({ label }: { label: string }) {
  return (
    <div role="columnheader">
      <div className="size-[16px]">
        <span className="sr-only">{label}</span>
      </div>
    </div>
  )
}

/**
 * Two lines in a cell: a medium title and a muted detail, as "test (node 22)"
 * over "kestrel-hq/api · ci.yml". `muted` dims the title, as for a cancelled job.
 */
export function CellTitle({ title, detail, muted = false }: { title: ReactNode; detail?: ReactNode; muted?: boolean }) {
  return (
    <div className="flex min-w-0 flex-col">
      <span className={cx('truncate text-s font-medium', muted ? 'text-neutral-7' : 'text-neutral-10')}>{title}</span>
      {detail && <span className="truncate text-xs text-neutral-7">{detail}</span>}
    </div>
  )
}

function RowAnchor({ className, ref, ...props }: AnchorHTMLAttributes<HTMLAnchorElement> & { ref?: Ref<HTMLAnchorElement> }) {
  return (
    <a
      ref={ref}
      data-row-link=""
      className={cx(
        'min-w-0 truncate outline-none',
        'after:absolute after:inset-zero after:outline-offset-[-2px] focus-visible:after:outline-2 focus-visible:after:outline-(--cte-focus)',
        className,
      )}
      {...props}
    />
  )
}

/** A router link that makes its whole row clickable. Use it once per row, around the row's name. */
export const RowLink = createLink(RowAnchor)

/** A column for useFittingColumns: its width, and for a column that may go, its drop level. */
export type FittingColumn = DataTableColumn | { width: DataTableColumn; drop?: number }

/**
 * The columns that fit a table, so that it does not scroll sideways. When the
 * table is too narrow for all of them, it drops the columns of drop level 1,
 * then those of level 2, and so on, until the rest fit. Columns without a level
 * always show: the name of the row and its controls. A column set to
 * undefined is not in this table. Put `ref` on an element around the
 * DataTable, give it `columns`, and render the cells of `shown`.
 *
 *   const fit = useFittingColumns({ name: 'grow', size: { width: 150, drop: 1 }, menu: 24 })
 */
export function useFittingColumns<K extends string>(spec: Record<K, FittingColumn | undefined>) {
  const [frame, setFrame] = useState({ width: 0, gap: 0 })
  // A callback ref, so a table that shows up later (after its data) is measured too.
  const ref = useCallback((element: HTMLDivElement | null) => {
    if (!element) return
    const gap = parseFloat(getComputedStyle(element).getPropertyValue('--space-m'))
    setFrame({ width: element.clientWidth, gap })
    const observer = new ResizeObserver(([entry]) => setFrame({ width: entry!.contentRect.width, gap }))
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  return { ref, ...fitColumns(spec, frame.width, frame.gap) }
}

/** The columns of `spec` that fit a table `width` pixels wide with `gap` between columns; all of them for width 0 (not measured yet). */
export function fitColumns<K extends string>(spec: Record<K, FittingColumn | undefined>, width: number, gap: number) {
  const all = (Object.entries(spec) as [K, FittingColumn | undefined][]).flatMap(([key, column]) => {
    if (column === undefined) return []
    return typeof column === 'object' ? { key, width: column.width, drop: column.drop ?? Infinity } : { key, width: column, drop: Infinity }
  })
  const maxDrop = Math.max(0, ...all.map((column) => column.drop).filter(Number.isFinite))
  // The table edge takes 1px on each side.
  const fits = (columns: readonly DataTableColumn[]) => columnsWidth(columns, 160) + (columns.length + 1) * gap <= width - 2
  let level = 0
  while (width > 0 && level < maxDrop && !fits(all.filter((column) => column.drop > level).map((column) => column.width))) level++
  const shown = {} as Record<K, boolean>
  for (const key in spec) shown[key] = false
  for (const column of all) shown[column.key] = column.drop > level
  return { shown, columns: all.filter((column) => shown[column.key]).map((column) => column.width) }
}

// Below this width the table scrolls sideways: the columns with 96px for each
// growing one, plus the M gaps and the M padding on both sides. Columns drop
// while a growing column would get less than 160px.
function minWidth(columns: readonly DataTableColumn[]): string {
  return `calc(${columnsWidth(columns, 96)}px + ${columns.length + 1} * var(--space-m))`
}

/** Fixed columns at full width, and `grow` pixels for each growing column. */
function columnsWidth(columns: readonly DataTableColumn[], grow: number): number {
  return columns.reduce<number>((sum, column) => sum + (column === 'grow' ? grow : column), 0)
}
