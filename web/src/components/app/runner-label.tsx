import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

/** A runs-on label such as "runplane-8cpu", in the code font as in the designs. */
export function RunnerLabel({ className, children }: { className?: string; children: ReactNode }) {
  return <span className={cx('truncate font-code text-xs tracking-m text-neutral-8', className)}>{children}</span>
}
