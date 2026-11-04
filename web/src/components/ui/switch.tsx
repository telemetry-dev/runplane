import { Field } from '@base-ui/react/field'
import { Switch as BaseSwitch } from '@base-ui/react/switch'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

type SwitchProps = Omit<BaseSwitch.Root.Props, 'className' | 'children'> & {
  /** Without a label, pass `aria-label`. */
  label?: ReactNode
  /** With a description the switch sits at the end of a settings row, as "Job access" in Settings · GitHub. */
  description?: ReactNode
  /** A message under the description of a settings row, for example a refused change. */
  error?: ReactNode
  className?: string
}

/** Switch of gui/components/runplane/switch.md: an 18px track with a 14px thumb, color-1 when on. */
export function Switch({ label, description, error, className, ...props }: SwitchProps) {
  if (description) {
    return (
      <Field.Root
        disabled={props.disabled}
        invalid={error ? true : undefined}
        className={cx('flex items-start justify-between gap-xl', className)}
      >
        <div className="flex min-w-0 flex-col gap-xxs">
          <Field.Label className="text-s font-medium text-neutral-10 data-disabled:text-neutral-6">{label}</Field.Label>
          <Field.Description className="text-s text-neutral-7 data-disabled:text-neutral-6">{description}</Field.Description>
          {error && (
            <Field.Error match className="text-xs text-error">
              {error}
            </Field.Error>
          )}
        </div>
        <Track {...props} />
      </Field.Root>
    )
  }
  if (label) {
    return (
      <label className={cx('inline-flex items-center gap-xs text-s font-medium text-neutral-10 has-data-disabled:text-neutral-6', className)}>
        <Track {...props} />
        {label}
      </label>
    )
  }
  return <Track className={className} {...props} />
}

function Track({ className, ...props }: Omit<BaseSwitch.Root.Props, 'className'> & { className?: string }) {
  return (
    <BaseSwitch.Root
      className={cx(
        'group relative h-[18px] w-[32px] shrink-0 rounded-full bg-neutral-4 transition-[background-color]',
        'data-checked:bg-color-1 data-disabled:cursor-not-allowed data-disabled:bg-neutral-3',
        className,
      )}
      {...props}
    >
      <BaseSwitch.Thumb
        className={cx(
          'absolute top-[2px] left-[2px] size-[14px] rounded-full bg-neutral-1 shadow-s transition-[translate,width]',
          'data-checked:translate-x-[14px] data-disabled:bg-neutral-6 data-disabled:shadow-none',
          // A press stretches the thumb by a quarter of its width; the far edge stays put.
          'group-[:active:not([data-disabled])]:w-[17.5px] group-[:active:not([data-disabled])]:data-checked:translate-x-[10.5px]',
          'motion-reduce:group-[:active]:w-[14px] motion-reduce:group-[:active]:data-checked:translate-x-[14px]',
        )}
      />
    </BaseSwitch.Root>
  )
}
