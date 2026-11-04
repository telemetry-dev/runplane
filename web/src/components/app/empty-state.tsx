import type { ReactNode } from 'react'

type EmptyStateProps = {
  title: ReactNode
  /** One or two sentences: what is missing and how to get it. */
  children?: ReactNode
  /** Buttons under the text, as "Copy label" and "See all labels". */
  actions?: ReactNode
  /** Content beside the text on wide screens, for example the CodeBlock that shows the change to make. */
  aside?: ReactNode
}

/** The bordered "No jobs yet" panel of the empty Overview. */
export function EmptyState({ title, children, actions, aside }: EmptyStateProps) {
  return (
    // The edge gets its own pixel of padding, as a border would.
    <section className="relative flex flex-col gap-xl rounded-m p-[calc(var(--space-xl)+1px)] after:pointer-events-none after:absolute after:inset-zero after:rounded-[inherit] after:shadow-[inset_0_0_0_1px_var(--neutral-4)] md:p-[calc(var(--space-xxl)+1px)] lg:flex-row lg:items-center lg:gap-xxl">
      <div className="flex flex-col gap-s lg:w-[380px] lg:shrink-0">
        <h2 className="type-page-title text-neutral-10">{title}</h2>
        {children && <div className="text-m tracking-l text-neutral-8">{children}</div>}
        {actions && <div className="mt-s flex flex-wrap gap-xs">{actions}</div>}
      </div>
      {aside && <div className="min-w-0 flex-1">{aside}</div>}
    </section>
  )
}
