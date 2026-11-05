import type { ButtonHTMLAttributes } from 'react'
import { cx } from '~/lib/cx'

/** A button that reads as a link inside a sentence, as "Try again" after an error. */
export function TextButton({ className, ...props }: ButtonHTMLAttributes<HTMLButtonElement>) {
  return <button type="button" className={cx('font-medium text-neutral-10 underline underline-offset-2', className)} {...props} />
}
