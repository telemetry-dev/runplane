import { Meter } from '@base-ui/react/meter'
import { Progress } from '@base-ui/react/progress'
import { formatPercent } from '~/lib/format'
import { cx } from '~/lib/cx'
import '~/components/ui/progress.css'

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

/** The thin color-1 bar of the tables: server CPU and memory, and job progress. */
export function UsageBar({ value, label, showValue = false, className }: UsageBarProps) {
  if (value === null) {
    return (
      <Progress.Root value={null} aria-label={label} className={cx('flex items-center', className)}>
        <Progress.Track className={track}>
          <Progress.Indicator className="progress-indicator h-full rounded-full bg-color-1" />
        </Progress.Track>
      </Progress.Root>
    )
  }
  const clamped = Math.min(100, Math.max(0, value))
  return (
    <Meter.Root value={clamped} aria-label={label} className={cx('flex items-center gap-s', className)}>
      <Meter.Track className={track}>
        <Meter.Indicator className="h-full rounded-full bg-color-1 transition-[width]" />
      </Meter.Track>
      {showValue && <span className="w-[4ch] shrink-0 text-xs text-neutral-8 tabular-nums">{formatPercent(clamped)}</span>}
    </Meter.Root>
  )
}

const track = 'h-[4px] min-w-0 flex-1 overflow-hidden rounded-full bg-neutral-3'
