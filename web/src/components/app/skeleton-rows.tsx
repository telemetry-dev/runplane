import type { ReactNode } from 'react'
import { Skeleton } from '~/components/ui/skeleton'
import { cx } from '~/lib/cx'
import { DataTableCell, DataTableRow } from './data-table'

type SkeletonRowsProps = {
  /** Read by screen readers while the rows show, as "Loading labels". */
  label?: string
  count?: number
  /** The cells of each row, as SkeletonCells. Without them, each row is one bar across the table. */
  children?: ReactNode
  /** Text lines of the bar across the table: 2 for rows with a detail line. */
  lines?: 1 | 2
}

/** Placeholder rows of a DataTable while its data loads, as tall as the rows that follow. */
export function SkeletonRows({ label, count = 3, children, lines = 1 }: SkeletonRowsProps) {
  return (
    <>
      {label && (
        // sr-only is absolute, so this row takes no place in the table.
        <div role="row" className="sr-only">
          <div role="cell">
            <span role="status">{label}</span>
          </div>
        </div>
      )}
      {Array.from({ length: count }, (_, index) => (
        <DataTableRow key={index}>
          {children ?? (
            <DataTableCell className="col-span-full">
              <span
                aria-hidden
                className={cx('flex w-full flex-col justify-center gap-xs', lines === 2 ? 'h-[calc(var(--line-s)+var(--line-xs))]' : 'h-(--line-s)')}
              >
                <Skeleton className="h-[8px]" style={{ width: `${32 - index * 6}%` }} />
                {lines === 2 && <Skeleton className="h-[6px]" style={{ width: `${18 - index * 3}%` }} />}
              </span>
            </DataTableCell>
          )}
        </DataTableRow>
      ))}
    </>
  )
}

type SkeletonCellProps = {
  /** Widths in pixels of the text lines of the cell: a title, and a detail line under it. */
  lines?: readonly [number] | readonly [number, number]
  /** A block before the text in the shape of an icon or a control. */
  shape?: keyof typeof shapes
  align?: 'start' | 'end'
}

/** A cell of SkeletonRows. Each bar sits on the line height of the text it stands for. */
export function SkeletonCell({ lines, shape, align }: SkeletonCellProps) {
  const [title, detail] = lines ?? []
  return (
    <DataTableCell align={align} className={cx(detail !== undefined && 'flex-col items-start gap-0', shape && title !== undefined && 'gap-s')}>
      {shape && <Skeleton className={shapes[shape]} />}
      {title !== undefined && <Skeleton className="my-[3px] h-[12px] max-w-full" style={{ width: title }} />}
      {detail !== undefined && <Skeleton className="my-[2px] h-[10px] max-w-full" style={{ width: detail }} />}
    </DataTableCell>
  )
}

const shapes = {
  icon: 'size-[16px] rounded-full',
  avatar: 'size-[24px] rounded-full',
  'large-avatar': 'size-[28px] rounded-full',
  tile: 'size-[28px] rounded-s',
  switch: 'h-[18px] w-[32px] rounded-full',
  select: 'h-[calc(var(--line-s)+2*var(--space-xs))] w-[120px] rounded-s',
}
