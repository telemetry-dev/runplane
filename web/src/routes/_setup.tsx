import { createFileRoute, Outlet } from '@tanstack/react-router'
import { ChildPending, frameDelayMs, useElapsed } from '~/components/shell/pending'
import { SetupFrame } from '~/components/shell/setup'

// Layout of the pages before sign-in: setup steps, login, and invites.
export const Route = createFileRoute('/_setup')({
  component: () => (
    <SetupFrame>
      <Outlet />
    </SetupFrame>
  ),
  pendingComponent: SetupPending,
  pendingMinMs: 0,
})

function SetupPending() {
  if (!useElapsed(frameDelayMs)) return null
  return (
    <SetupFrame>
      <ChildPending layout={Route.id} />
    </SetupFrame>
  )
}
