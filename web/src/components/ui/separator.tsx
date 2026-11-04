import { Separator as BaseSeparator } from '@base-ui/react/separator'
import { cx } from '~/lib/cx'

type SeparatorProps = Omit<BaseSeparator.Props, 'className'> & { className?: string }

/**
 * Separator of gui/components/runplane/separator.md: a 1px neutral-4 line drawn
 * as an inset edge in an XXS band, so a thicker theme edge never moves content.
 */
export function Separator({ orientation = 'horizontal', className, ...props }: SeparatorProps) {
  return (
    <BaseSeparator
      orientation={orientation}
      className={cx(
        'shrink-0',
        orientation === 'horizontal'
          ? 'h-xxs w-full min-w-m shadow-[inset_0_1px_0_var(--neutral-4)]'
          : 'w-xxs min-h-l self-stretch shadow-[inset_1px_0_0_var(--neutral-4)]',
        className,
      )}
      {...props}
    />
  )
}
