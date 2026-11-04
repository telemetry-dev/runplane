import { Checkbox as BaseCheckbox } from '@base-ui/react/checkbox'
import { CheckboxGroup as BaseCheckboxGroup } from '@base-ui/react/checkbox-group'
import { Check, Minus } from 'lucide-react'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

type CheckboxProps = Omit<BaseCheckbox.Root.Props, 'className' | 'children'> & {
  /** Without a label, pass `aria-label`. */
  label?: ReactNode
  className?: string
}

/**
 * Checkbox of gui/components/runplane/checkbox.md. The color-1 fill grows from
 * the center, then the mark scales in. Inside a CheckboxGroup, pass `value`;
 * a `parent` checkbox checks all of the group's `allValues`.
 */
export function Checkbox({ label, className, ...props }: CheckboxProps) {
  if (!label) return <Control className={className} {...props} />
  return (
    <label className={cx('inline-flex items-center gap-xs text-s text-neutral-10 has-data-disabled:text-neutral-6', className)}>
      <Control {...props} />
      {label}
    </label>
  )
}

type CheckboxGroupProps = Omit<BaseCheckboxGroup.Props, 'className'> & { className?: string }

/** A vertical list of labeled Checkboxes that share one array value. */
export function CheckboxGroup({ className, ...props }: CheckboxGroupProps) {
  return <BaseCheckboxGroup className={cx('flex flex-col items-start gap-s', className)} {...props} />
}

// 16px like the radio of the designs, with the same 1.5px inset edge, because the
// theme's 0px edge would leave an unchecked box invisible on a white surface.
function Control({ className, ...props }: Omit<BaseCheckbox.Root.Props, 'className'> & { className?: string }) {
  return (
    <BaseCheckbox.Root
      className={cx(
        'relative grid size-[16px] shrink-0 place-items-center rounded-xs bg-neutral-1',
        'shadow-[inset_0_0_0_1.5px_var(--neutral-5)] transition-[box-shadow] not-data-disabled:hover:shadow-[inset_0_0_0_1.5px_var(--neutral-7)]',
        'before:absolute before:inset-zero before:scale-0 before:rounded-xs before:bg-color-1 before:transition-[scale]',
        'data-checked:before:scale-100 data-indeterminate:before:scale-100',
        'data-disabled:cursor-not-allowed data-disabled:bg-neutral-3 data-disabled:shadow-[inset_0_0_0_1.5px_var(--neutral-4)] data-disabled:before:bg-neutral-6',
        className,
      )}
      {...props}
    >
      <BaseCheckbox.Indicator
        render={(indicatorProps, state) => (
          <span {...indicatorProps}>{state.indeterminate ? <Minus aria-hidden /> : <Check aria-hidden />}</span>
        )}
        className={cx(
          'relative flex text-neutral-1 [--icon-stroke-width:2.5] [&_svg]:size-[11px]',
          'transition-[scale] delay-[calc(var(--motion-duration)*0.35)] duration-[calc(var(--motion-duration)*0.9)]',
          'data-starting-style:scale-0 data-ending-style:scale-0',
          'data-ending-style:delay-0 data-ending-style:duration-[calc(var(--motion-duration)*0.7)]',
        )}
      />
    </BaseCheckbox.Root>
  )
}
