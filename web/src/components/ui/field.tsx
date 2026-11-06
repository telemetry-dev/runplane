import { Field as BaseField } from '@base-ui/react/field'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

// Field compositions put an Input inside a Field; both come from here.
export { Input } from './input'

type FieldProps = Omit<BaseField.Root.Props, 'className' | 'children'> & {
  label: ReactNode
  description?: ReactNode
  /** Lets the description and the error use the full width of the field instead of 40 characters. */
  wide?: boolean
  className?: string
  children: ReactNode
}

/** Field of gui/components/runplane/field.md. */
export function Field({ label, description, wide = false, className, children, ...props }: FieldProps) {
  return (
    <BaseField.Root className={cx('flex flex-col gap-xs', className)} {...props}>
      <BaseField.Label className="text-s font-medium text-neutral-10 data-disabled:text-neutral-6">{label}</BaseField.Label>
      {children}
      {description && (
        <BaseField.Description className={cx('text-s text-neutral-7 data-disabled:text-neutral-6', !wide && 'max-w-[40ch]')}>
          {description}
        </BaseField.Description>
      )}
      <BaseField.Error className={cx('text-xs text-error data-disabled:text-neutral-6', !wide && 'max-w-[40ch]')} />
    </BaseField.Root>
  )
}
