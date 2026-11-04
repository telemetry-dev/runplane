import { Button as BaseButton } from '@base-ui/react/button'
import { cx } from '~/lib/cx'
import { Spinner } from './spinner'
import { Tooltip } from './tooltip'

export type ButtonVariant = 'primary' | 'secondary' | 'outline' | 'ghost' | 'danger'

type ButtonProps = Omit<BaseButton.Props, 'className'> & {
  variant?: ButtonVariant
  /** Shows a spinner and blocks presses; the button keeps focus while the action runs. */
  loading?: boolean
  className?: string
}

/** Button of gui/components/runplane/button.md. Icons inside it are 14px, as in the designs. */
export function Button({ variant = 'secondary', loading = false, disabled, className, children, ...props }: ButtonProps) {
  return (
    <BaseButton
      className={buttonClassName(variant, className)}
      disabled={disabled || loading}
      focusableWhenDisabled={loading}
      aria-busy={loading || undefined}
      {...props}
    >
      {loading && <Spinner size={14} />}
      {children}
    </BaseButton>
  )
}

/** Button styles for elements that are not buttons, for example a router Link. */
export function buttonClassName(variant: ButtonVariant = 'secondary', className?: string): string {
  return cx(base, variants[variant], className)
}

const base = cx(
  'inline-flex items-center justify-center gap-xs rounded-s px-m py-xs',
  'text-s font-medium whitespace-nowrap select-none',
  'transition-[background-color,color,box-shadow,translate]',
  'not-data-disabled:active:translate-y-(--motion-press-distance)',
  'data-disabled:cursor-not-allowed data-disabled:text-neutral-6',
  '[&_svg]:size-[14px]',
)

const variants: Record<ButtonVariant, string> = {
  primary: 'bg-neutral-10 text-neutral-1 not-data-disabled:hover:shadow-s data-disabled:bg-neutral-3',
  secondary: 'bg-neutral-3 text-neutral-10 not-data-disabled:hover:bg-neutral-4-transparent',
  outline: 'text-neutral-10 not-data-disabled:hover:bg-neutral-3-transparent',
  ghost: 'text-neutral-10 not-data-disabled:hover:bg-neutral-3-transparent',
  danger: 'bg-error text-neutral-1 not-data-disabled:hover:shadow-s data-disabled:bg-neutral-3',
}

type IconButtonProps = Omit<BaseButton.Props, 'className' | 'aria-label'> & {
  /** Accessible name, also shown as a tooltip. */
  label: string
  /** `secondary` gives the neutral-3 fill and the height of a secondary Button, as the "More" button next to page actions. */
  variant?: 'ghost' | 'secondary'
  className?: string
}

/**
 * Square button around one 16px icon, as the log download, dialog close, and
 * row "more" buttons of the designs. Menu and dialog triggers can render it:
 * `<MenuTrigger render={<IconButton label="More" />}>`.
 */
export function IconButton({ label, variant = 'ghost', className, ...props }: IconButtonProps) {
  return (
    <Tooltip content={label}>
      <BaseButton
        aria-label={label}
        className={cx(
          'inline-grid shrink-0 place-items-center rounded-s transition-[background-color,color]',
          'data-disabled:cursor-not-allowed data-disabled:text-neutral-5 [&_svg]:size-[16px]',
          iconVariants[variant],
          className,
        )}
        {...props}
      />
    </Tooltip>
  )
}

const iconVariants = {
  ghost: 'p-xs text-neutral-7 not-data-disabled:hover:bg-neutral-3 not-data-disabled:hover:text-neutral-10 data-popup-open:bg-neutral-3',
  // As tall as a Button (its line height plus XS padding on both sides), with the hover of a secondary Button.
  secondary: cx(
    'size-[calc(var(--line-s)+2*var(--space-xs))] bg-neutral-3 text-neutral-10',
    'not-data-disabled:hover:bg-neutral-4-transparent data-popup-open:bg-neutral-4-transparent',
  ),
}
