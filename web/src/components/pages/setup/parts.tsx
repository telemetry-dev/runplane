import type { ReactNode } from 'react'
import { StatusIcon } from '~/components/app/status-icon'

// Parts shared by the setup, log in, and invite pages.

/** The page title, with an optional line of context under it. */
export function SetupHeader({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <header className="flex flex-col gap-s">
      <h1 className="type-display text-neutral-10">{title}</h1>
      {children && <p className="text-m tracking-l text-neutral-8">{children}</p>}
    </header>
  )
}

/** The grey panel that holds the fields of a form. Its inputs are neutral-1, so they stand out on it. */
export function FormPanel({ children }: { children: ReactNode }) {
  return <div className="flex flex-col gap-xl rounded-m bg-neutral-2 p-xl">{children}</div>
}

/** A problem with the whole form, as a wrong password. It goes last in the FormPanel. */
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
