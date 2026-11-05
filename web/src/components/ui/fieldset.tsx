import { Fieldset as BaseFieldset } from '@base-ui/react/fieldset'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

type FieldsetProps = Omit<BaseFieldset.Root.Props, 'className' | 'children'> & {
  legend: ReactNode
  className?: string
  children: ReactNode
}

/**
 * A group of controls under one legend (gui/components/runplane/fieldset.md).
 * `disabled` disables every control inside.
 */
export function Fieldset({ legend, className, children, ...props }: FieldsetProps) {
  return (
    <BaseFieldset.Root className={cx('flex min-w-0 flex-col gap-s', className)} {...props}>
      <BaseFieldset.Legend className="text-s font-medium text-neutral-10 data-disabled:text-neutral-6">{legend}</BaseFieldset.Legend>
      {children}
    </BaseFieldset.Root>
  )
}
