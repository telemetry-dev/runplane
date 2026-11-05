import { createFileRoute, Outlet } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { useLiveEvents } from '~/api/events'
import { githubQuery } from '~/api/github'
import { runningJobsQuery } from '~/api/jobs'
import { requireSession, sessionQuery, statusQuery } from '~/api/session'
import { AppSidebar } from '~/components/shell/app-sidebar'
import { ChildPending, frameDelayMs, PendingFrame, useCachedData, useElapsed } from '~/components/shell/pending'

// Layout of all signed-in pages: sidebar and the content column of the app designs.
export const Route = createFileRoute('/_app')({
  beforeLoad: async ({ context: { queryClient }, location }) => {
    await requireSession(queryClient, location.href)
  },
  // Starts the sidebar's requests next to the page's, so a pending frame can show their data.
  loader: ({ context: { queryClient } }) => {
    void queryClient.prefetchQuery({ ...githubQuery, staleTime: Infinity })
    void queryClient.prefetchQuery({ ...runningJobsQuery, staleTime: Infinity })
  },
  component: AppLayout,
  pendingComponent: AppPending,
  // The page's pending UI holds itself; a longer minimum would only delay every direct page load.
  pendingMinMs: 0,
})

function AppLayout() {
  useLiveEvents()
  return (
    <AppFrame>
      <Outlet />
    </AppFrame>
  )
}

function AppPending() {
  // The checks of beforeLoad: until they pass, the visitor may still go to setup or login.
  const status = useCachedData(statusQuery)
  const user = useCachedData(sessionQuery)
  const shown = useElapsed(frameDelayMs)
  if (status?.needs_setup !== false || !user || !shown) return null
  return (
    <PendingFrame value>
      <AppFrame>
        <ChildPending layout={Route.id} />
      </AppFrame>
    </PendingFrame>
  )
}

function AppFrame({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-dvh flex-col md:flex-row">
      <a
        href="#content"
        // Focus without a #content in the URL, which the router would keep.
        onClick={(event) => {
          event.preventDefault()
          document.getElementById('content')?.focus()
        }}
        // Above the viewport until it has the focus.
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
