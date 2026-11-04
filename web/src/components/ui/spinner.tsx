import { cx } from '~/lib/cx'

type SpinnerProps = {
  /** Pixels. The designs use 14 in steps, 16 in tables, and 22 next to a page title. */
  size?: number
  /** Accessible name. Without it the spinner is decorative and the text next to it carries the state. */
  label?: string
  className?: string
}

/** The color-1 arc on a neutral-4 ring of the designs ("running", "installing", "waiting"). */
export function Spinner({ size = 16, label, className }: SpinnerProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={cx('shrink-0 animate-spin', className)}
      role={label ? 'img' : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    >
      <circle cx="12" cy="12" r="9" fill="none" stroke="var(--neutral-4)" strokeWidth="2.25" />
      <path d="M12 3a9 9 0 0 1 9 9" fill="none" stroke="var(--color-1)" strokeWidth="2.25" strokeLinecap="round" />
    </svg>
  )
}
