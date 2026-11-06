import { Collapsible as BaseCollapsible } from '@base-ui/react/collapsible'
import { ChevronRight } from 'lucide-react'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

type CollapsibleProps = Omit<BaseCollapsible.Root.Props, 'className' | 'children'> & {
  /** The trigger's label. */
  title: ReactNode
  className?: string
  children: ReactNode
}

/** Collapsible of gui/components/runplane/collapsible.md. */
export function Collapsible({ title, className, children, ...props }: CollapsibleProps) {
  return (
    <BaseCollapsible.Root className={cx('group/collapsible flex flex-col', className)} {...props}>
      <BaseCollapsible.Trigger className="flex w-full items-center justify-between gap-xs rounded-s py-xs text-left text-s font-medium text-neutral-10 data-disabled:text-neutral-6">
        {title}
        <ChevronRight
          size={16}
          aria-hidden
          className="text-neutral-7 transition-[rotate] duration-(--motion-large-duration) ease-large group-data-open/collapsible:rotate-90"
        />
      </BaseCollapsible.Trigger>
      <BaseCollapsible.Panel
        className={cx(
          'h-(--collapsible-panel-height) overflow-hidden rounded-s bg-neutral-2 opacity-100',
          'transition-[height,opacity,margin] duration-(--motion-large-duration) ease-large',
          'mt-s data-starting-style:mt-zero data-ending-style:mt-zero',
          'data-starting-style:h-0 data-starting-style:opacity-0 data-ending-style:h-0 data-ending-style:opacity-0',
        )}
      >
        <div className="flow-root p-s text-s text-neutral-8">{children}</div>
      </BaseCollapsible.Panel>
    </BaseCollapsible.Root>
  )
}
