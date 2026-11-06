import type { ReactNode } from 'react'
import { AppSidebar } from './app-sidebar'

export function AppFrame({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-dvh flex-col md:flex-row">
      <a
        href="#content"
        // Focus without a #content in the URL, which the router would keep.
        onClick={(event) => {
          event.preventDefault()
          document.getElementById('content')?.focus()
        }}
        className="fixed top-s left-s z-30 -translate-y-[calc(100%+var(--space-xl))] rounded-s bg-neutral-1 px-m py-xs text-s font-medium text-neutral-10 shadow-[0_0_0_1px_var(--neutral-4),var(--shadow-m)] focus:translate-y-0"
      >
        Skip to content
      </a>
      <AppSidebar />
      <main id="content" tabIndex={-1} className="flex min-w-0 flex-1 flex-col gap-xxl px-l pt-l pb-xxl md:px-xxl md:pt-xl">
        {children}
      </main>
    </div>
  )
}
