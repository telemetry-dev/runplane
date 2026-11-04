import type { ReactNode } from 'react'

type StatProps = {
  label: ReactNode
  /** The figure. `null` or `undefined` shows an em dash, for a stat without data yet. */
  value: ReactNode
  /** A muted suffix, as "/ 80" in "36 / 80" for vCPU in use. */
  total?: ReactNode
}

/** A label over a large number, as the stats row at the top of Overview. */
export function Stat({ label, value, total }: StatProps) {
  const empty = value === null || value === undefined
  return (
    <div className="flex min-w-0 flex-col gap-xs">
      <span className="truncate text-xs font-medium text-neutral-7">{label}</span>
      <span className="type-stat truncate text-neutral-10">
        {empty ? (
          <span className="text-neutral-5" aria-label="No data">
            —
          </span>
        ) : (
          value
        )}
        {!empty && total !== undefined && <span className="text-neutral-6"> / {total}</span>}
      </span>
    </div>
  )
}
