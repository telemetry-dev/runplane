import type { ReactNode } from 'react'

type PageHeaderProps = {
  title: ReactNode
  /** Shown after the title in regular weight, as in "Servers 6". */
  count?: number
  /** Before the title, as the 22px StatusIcon of Job detail. */
  icon?: ReactNode
  /** Buttons on the right. */
  actions?: ReactNode
}

/** The 36px title row at the top of each app page. */
export function PageHeader({ title, count, icon, actions }: PageHeaderProps) {
  return (
    <header className="flex min-h-[36px] shrink-0 flex-wrap items-center justify-between gap-m">
      <div className="flex min-w-0 items-center gap-s">
        {icon}
        <div className="flex min-w-0 items-baseline gap-xs">
          <h1 className="type-page-title truncate text-neutral-10">{title}</h1>
          {count !== undefined && <span className="text-l font-regular tracking-m text-neutral-6">{count}</span>}
        </div>
      </div>
      {actions && <div className="flex flex-wrap items-center gap-xs">{actions}</div>}
    </header>
  )
}
