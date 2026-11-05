import { createFileRoute, Outlet } from '@tanstack/react-router'
import { useLiveEvents } from '~/api/events'
import { requireSession, sessionQuery, statusQuery } from '~/api/session'
import { AppFrame } from '~/components/shell/app-frame'
import { prefetchSidebar } from '~/components/shell/app-sidebar'
import { ChildPending, frameDelayMs, PendingFrame, useCachedData, useElapsed } from '~/components/shell/pending'

export const Route = createFileRoute('/_app')({
  beforeLoad: async ({ context: { queryClient }, location }) => {
    await requireSession(queryClient, location.href)
  },
  loader: ({ context: { queryClient } }) => prefetchSidebar(queryClient),
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
