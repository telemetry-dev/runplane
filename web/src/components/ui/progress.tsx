import { Progress as BaseProgress } from '@base-ui/react/progress'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'
import './progress.css'

type ProgressProps = Omit<BaseProgress.Root.Props, 'className' | 'children'> & {
  /** Shown above the track with the formatted value. Without it, pass `aria-label`. */
  label?: ReactNode
  className?: string
}

/**
 * Progress of gui/components/runplane/progress.md: a 3px color-1 bar. A `null`
 * value is indeterminate work; the bar then sweeps and no number shows.
 */
export function Progress({ label, className, ...props }: ProgressProps) {
  return (
    <BaseProgress.Root className={cx('flex w-full flex-col gap-s', className)} {...props}>
      {label && (
        <div className="flex items-baseline justify-between gap-m">
          <BaseProgress.Label className="text-s font-medium text-neutral-10">{label}</BaseProgress.Label>
          <BaseProgress.Value className="text-xs text-neutral-7 tabular-nums" />
        </div>
      )}
      <BaseProgress.Track className="h-[3px] overflow-hidden rounded-full bg-neutral-3">
        <BaseProgress.Indicator className="progress-indicator h-full rounded-full bg-color-1 transition-[width]" />
      </BaseProgress.Track>
    </BaseProgress.Root>
  )
}
