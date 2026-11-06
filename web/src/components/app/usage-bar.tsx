import { Meter } from '~/components/ui/meter'
import { Progress } from '~/components/ui/progress'
import { cx } from '~/lib/cx'
import { formatPercent } from '~/lib/format'

type UsageBarProps = {
  /** 0 to 100. `null` is unknown progress; the bar then sweeps. */
  value: number | null
  /** Accessible name, for example "CPU" or "Progress". */
  label: string
  /** Shows the percent after the bar, as in the Servers tables. */
  showValue?: boolean
  /** Classes of the whole bar; set its width here (the designs use 88 to 132px). */
  className?: string
}

/** The bar of the tables: server CPU and memory, and job progress. */
export function UsageBar({ value, label, showValue = false, className }: UsageBarProps) {
  if (value === null) return <Progress value={null} aria-label={label} className={className} />
  const clamped = Math.min(100, Math.max(0, value))
  return (
    <div className={cx('flex items-center gap-s', className)}>
      <Meter value={clamped} aria-label={label} className="min-w-0 flex-1" />
      {showValue && <span className="w-[4ch] shrink-0 text-xs text-neutral-8 tabular-nums">{formatPercent(clamped)}</span>}
    </div>
  )
}
