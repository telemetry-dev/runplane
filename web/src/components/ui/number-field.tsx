import { Field } from '@base-ui/react/field'
import { NumberField as BaseNumberField } from '@base-ui/react/number-field'
import { Minus, Plus } from 'lucide-react'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

type NumberFieldProps = Omit<BaseNumberField.Root.Props, 'className' | 'children'> & {
  /** Without a label, pass `aria-label`, or put the field inside a Field. */
  label?: ReactNode
  className?: string
}

/**
 * Number field of gui/components/runplane/number-field.md: minus, an 80px input,
 * plus. Holding a stepper repeats; min, max, step, and parsing stay in Base UI.
 */
export function NumberField({ label, className, ...props }: NumberFieldProps) {
  const control = (
    <BaseNumberField.Root className={cx(!label && className)} {...props}>
      <BaseNumberField.Group className="flex items-center gap-xs">
        <BaseNumberField.Decrement aria-label="Decrease" className={stepper}>
          <Minus size="1em" aria-hidden />
        </BaseNumberField.Decrement>
        <BaseNumberField.Input
          className={cx(
            'w-[80px] rounded-s bg-(--input-background,var(--neutral-1)) px-s py-xs text-center text-s text-neutral-10 tabular-nums',
            'transition-[background-color,color] read-only:bg-neutral-2 read-only:text-neutral-8',
            'data-disabled:bg-neutral-3 data-disabled:text-neutral-6',
          )}
        />
        <BaseNumberField.Increment aria-label="Increase" className={stepper}>
          <Plus size="1em" aria-hidden />
        </BaseNumberField.Increment>
      </BaseNumberField.Group>
    </BaseNumberField.Root>
  )
  if (!label) return control
  return (
    <Field.Root disabled={props.disabled} className={cx('flex flex-col gap-xs', className)}>
      <Field.Label className="text-s font-medium text-neutral-10 data-disabled:text-neutral-6">{label}</Field.Label>
      {control}
    </Field.Root>
  )
}

// A square as tall as the input: its line height plus XS padding on both sides.
const stepper = cx(
  'grid size-[calc(var(--line-s)+2*var(--space-xs))] shrink-0 place-items-center rounded-full bg-neutral-1 text-s font-medium text-neutral-10 shadow-s',
  'transition-[background-color,color] [&_svg]:transition-[scale] not-data-disabled:active:[&_svg]:scale-75',
  'data-disabled:cursor-not-allowed data-disabled:bg-neutral-2 data-disabled:text-neutral-6 data-disabled:shadow-none',
)

type NumberInputProps = Omit<BaseNumberField.Root.Props, 'className' | 'children' | keyof InputProps> &
  InputProps & {
    /** Unit after the value, as "vCPU" or "GB". It is not part of the input's name, so name it in the label too where it matters. */
    suffix?: ReactNode
    /** On the root; the field fills its parent unless this sets a width. */
    className?: string
  }

// These go to the input element; the root takes the other props.
type InputProps = Pick<BaseNumberField.Input.Props, 'placeholder' | 'aria-label' | 'onKeyDown' | 'autoFocus'>

/**
 * A number input in the text input style, without steppers, as the size fields
 * of the label dialog and the number settings. Arrow keys still step. Put it
 * inside a Field for the label and the error.
 */
export function NumberInput({ suffix, className, placeholder, 'aria-label': label, onKeyDown, autoFocus, ...props }: NumberInputProps) {
  return (
    <BaseNumberField.Root className={className} {...props}>
      {/* A label, so a press on the padding or the suffix focuses the input. */}
      <BaseNumberField.Group
        render={<label />}
        className={cx(
          'flex cursor-text items-center gap-xs rounded-s bg-(--input-background,var(--neutral-1)) px-s py-xs text-s',
          'transition-[background-color] has-focus-visible:outline-2 has-focus-visible:outline-(--cte-focus)',
          'data-disabled:cursor-not-allowed data-disabled:bg-neutral-3',
        )}
      >
        <BaseNumberField.Input
          placeholder={placeholder}
          aria-label={label}
          onKeyDown={onKeyDown}
          autoFocus={autoFocus}
          className="w-full min-w-0 bg-transparent text-neutral-10 tabular-nums outline-none placeholder:text-neutral-7 data-disabled:text-neutral-6"
        />
        {suffix && (
          <span className="shrink-0 text-neutral-7 select-none">
            {suffix}
          </span>
        )}
      </BaseNumberField.Group>
    </BaseNumberField.Root>
  )
}
