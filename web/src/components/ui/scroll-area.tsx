import { ScrollArea as BaseScrollArea } from '@base-ui/react/scroll-area'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

type ScrollAreaProps = {
  /** Classes of the outer frame, for example a height such as `h-[240px]`. */
  className?: string
  /** Classes of the scrolling content, for example its padding. */
  contentClassName?: string
  children: ReactNode
}

/** Scroll area of gui/components/runplane/scroll-area.md. */
export function ScrollArea({ className, contentClassName, children }: ScrollAreaProps) {
  return (
    <BaseScrollArea.Root className={cx('relative min-h-zero overflow-hidden', className)}>
      <BaseScrollArea.Viewport className="size-full overscroll-contain rounded-[inherit] outline-none">
        <BaseScrollArea.Content className={contentClassName}>{children}</BaseScrollArea.Content>
      </BaseScrollArea.Viewport>
      <Scrollbar orientation="vertical" />
      <Scrollbar orientation="horizontal" />
      <BaseScrollArea.Corner />
    </BaseScrollArea.Root>
  )
}

function Scrollbar({ orientation }: { orientation: 'vertical' | 'horizontal' }) {
  return (
    <BaseScrollArea.Scrollbar
      orientation={orientation}
      className={cx(
        'm-xxs flex rounded-full select-none',
        orientation === 'vertical' ? 'w-[6px]' : 'h-[6px] flex-col',
      )}
    >
      <BaseScrollArea.Thumb className="flex-1 rounded-full bg-neutral-4 transition-colors hover:bg-neutral-5" />
    </BaseScrollArea.Scrollbar>
  )
}
