import { Radio as BaseRadio } from '@base-ui/react/radio'
import { RadioGroup as BaseRadioGroup } from '@base-ui/react/radio-group'
import type { ReactNode } from 'react'
import { cx } from '~/lib/cx'

// RadioGroup, Radio, and RadioCard of gui/components/runplane/radio-group.md and
// the "Install on" choice of the Connect GitHub design:
//
//   <RadioGroup name="account_type" defaultValue="organization" direction="row" aria-label="Install on">
//     <RadioCard value="organization" title="Organization" description="Every repository …" />
//     <RadioCard value="user" title="Personal account" description="Only repositories you own …" />
//   </RadioGroup>

type RadioGroupProps = Omit<BaseRadioGroup.Props, 'className'> & {
  /** `row` puts the options side by side with equal widths, as the radio cards of the designs. */
  direction?: 'column' | 'row'
  className?: string
}

export function RadioGroup({ direction = 'column', className, ...props }: RadioGroupProps) {
  return (
    <BaseRadioGroup
      className={cx(
        'flex gap-s',
        direction === 'row' ? 'flex-row flex-wrap *:min-w-[200px] *:flex-1' : 'flex-col',
        className,
      )}
      {...props}
    />
  )
}

type RadioProps = Omit<BaseRadio.Root.Props, 'className' | 'children'> & {
  /** Without a label, pass `aria-label`. */
  label?: ReactNode
  className?: string
}

export function Radio({ label, className, ...props }: RadioProps) {
  if (!label) return <Control className={className} {...props} />
  return (
    <label className={cx('inline-flex items-center gap-xs text-s text-neutral-10 has-data-disabled:text-neutral-6', className)}>
      <Control {...props} />
      {label}
    </label>
  )
}

type RadioCardProps = Omit<BaseRadio.Root.Props, 'className' | 'children'> & {
  title: ReactNode
  description?: ReactNode
  className?: string
}

/** A whole card selects the option. The fill is neutral-1, for use on a neutral-2 panel. */
export function RadioCard({ title, description, className, ...props }: RadioCardProps) {
  return (
    <label
      className={cx(
        'group flex cursor-pointer gap-s rounded-s bg-(--input-background,var(--neutral-1)) p-m',
        'has-data-disabled:cursor-not-allowed',
        className,
      )}
    >
      <Control className="mt-[1px]" {...props} />
      <span className="flex min-w-0 flex-col gap-xxs">
        <span className="text-s font-medium text-neutral-10 group-has-data-disabled:text-neutral-6">{title}</span>
        {description && <span className="text-xs text-neutral-7 group-has-data-disabled:text-neutral-6">{description}</span>}
      </span>
    </label>
  )
}

// The designs draw a 16px circle with a 1.5px edge and a 7px dot. The theme's edge
// width is 0, so the edge is an inset shadow and does not change the size.
function Control({ className, ...props }: Omit<BaseRadio.Root.Props, 'className'> & { className?: string }) {
  return (
    <BaseRadio.Root
      className={cx(
        'grid size-[16px] shrink-0 place-items-center rounded-full bg-neutral-1 transition-[box-shadow,background-color]',
        'shadow-[inset_0_0_0_1.5px_var(--neutral-5)] not-data-disabled:not-data-checked:hover:shadow-[inset_0_0_0_1.5px_var(--neutral-7)]',
        'data-checked:shadow-[inset_0_0_0_1.5px_var(--color-1)]',
        'data-disabled:cursor-not-allowed data-disabled:bg-neutral-3 data-disabled:shadow-[inset_0_0_0_1.5px_var(--neutral-4)]',
        className,
      )}
      {...props}
    >
      <BaseRadio.Indicator className="size-[7px] rounded-full bg-color-1 data-disabled:bg-neutral-6 data-unchecked:hidden" />
    </BaseRadio.Root>
  )
}
