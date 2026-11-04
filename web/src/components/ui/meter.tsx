import { Meter as BaseMeter } from '@base-ui/react/meter'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

type MeterProps = Omit<BaseMeter.Root.Props, 'className' | 'children'> & {
  /** Shown above the track with the formatted value. Without it, pass `aria-label`. */
  label?: ReactNode
  className?: string
}

/**
 * Meter of gui/components/runplane/meter.md: a read-only measurement such as disk
 * use, drawn as a 3px color-1 bar. For the compact table bars use UsageBar.
 */
export function Meter({ label, className, ...props }: MeterProps) {
  return (
    <BaseMeter.Root className={cx('flex w-full flex-col gap-s', className)} {...props}>
      {label && (
        <div className="flex items-baseline justify-between gap-m">
          <BaseMeter.Label className="text-s font-medium text-neutral-10">{label}</BaseMeter.Label>
          <BaseMeter.Value className="text-xs text-neutral-7 tabular-nums" />
        </div>
      )}
      <BaseMeter.Track className="h-[3px] overflow-hidden rounded-full bg-neutral-3">
        <BaseMeter.Indicator className="h-full rounded-full bg-color-1 transition-[width]" />
      </BaseMeter.Track>
    </BaseMeter.Root>
  )
}
