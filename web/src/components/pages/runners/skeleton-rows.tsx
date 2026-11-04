import { DataTableCell, DataTableRow } from '~/components/app/data-table'
import { cx } from '~/lib/cx'

type SkeletonRowsProps = {
  /** Text lines of a real row, so the placeholder rows have its height. */
  lines?: 1 | 2
  label: string
}

/** Three placeholder rows of a DataTable while its data loads. */
export function SkeletonRows({ lines = 1, label }: SkeletonRowsProps) {
  return [0, 1, 2].map((index) => (
    <DataTableRow key={index}>
      <DataTableCell className="col-span-full">
        {index === 0 && (
          <span role="status" className="sr-only">
            {label}
          </span>
        )}
        <span
          aria-hidden
          className={cx('flex w-full flex-col justify-center gap-xs', lines === 2 ? 'h-[calc(var(--line-s)+var(--line-xs))]' : 'h-(--line-s)')}
        >
          <span className={cx(bar, 'h-[8px]')} style={{ width: `${32 - index * 6}%` }} />
          {lines === 2 && <span className={cx(bar, 'h-[6px]')} style={{ width: `${18 - index * 3}%` }} />}
        </span>
      </DataTableCell>
    </DataTableRow>
  ))
}

const bar = 'block animate-pulse rounded-xs bg-neutral-3'
