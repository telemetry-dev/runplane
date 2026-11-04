import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

/** A label over a value, as Commit, Branch, Runner … in Job detail. */
export function KeyValue({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-xxs">
      <dt className="truncate text-xs font-medium text-neutral-7">{label}</dt>
      <dd className="min-w-0 text-s text-neutral-10">{children}</dd>
    </div>
  )
}

/** A row of KeyValues that wraps on narrow screens. Job detail spaces them XXL apart; pass `gap-x-xl` for a card. */
export function KeyValueList({ className, children }: { className?: string; children: ReactNode }) {
  return <dl className={cx('flex flex-wrap gap-x-xxl gap-y-m', className)}>{children}</dl>
}
