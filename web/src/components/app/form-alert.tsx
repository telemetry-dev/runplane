import type { ReactNode } from 'react'
import { StatusIcon } from './status-icon'

/** A problem with the whole form, as a wrong password. It goes last in the form. */
export function FormAlert({ children }: { children: ReactNode }) {
  return (
    <p role="alert" className="flex items-start gap-s text-s font-medium text-error">
      <span aria-hidden className="flex h-[18px] w-[14px] shrink-0 items-center">
        <StatusIcon status="failed" size={14} />
      </span>
      <span className="min-w-0">{children}</span>
    </p>
  )
}
