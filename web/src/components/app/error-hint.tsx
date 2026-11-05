import type { ReactNode } from 'react'
import { Tooltip } from '~/components/ui/tooltip'
import { cx } from '~/lib/cx'

type ErrorHintProps = {
  /** The message. Without one, only the children show. */
  error: string
  /** Classes of the trigger, as a dotted underline for text. */
  className?: string
  children: ReactNode
}

/**
 * Shows an error message in a tooltip on a status, as a failed image pull. The
 * trigger takes keyboard focus, and screen readers read the message with it.
 */
export function ErrorHint({ error, className, children }: ErrorHintProps) {
  if (!error) return children
  return (
    <Tooltip content={error}>
      <span tabIndex={0} className={cx('flex min-w-0 cursor-default items-center rounded-xs', className)}>
        {children}
        <span className="sr-only">: {error}</span>
      </span>
    </Tooltip>
  )
}
