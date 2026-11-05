import { createFileRoute, Outlet, redirect, useLocation } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { requireSession, statusQuery } from '~/api/session'
import { ChildPending } from '~/components/shell/pending'
import { SetupColumn, setupStep, SetupStepper } from '~/components/shell/setup'

// Step 1 is open only until the first admin exists; steps 2 and 3 need that admin's session.
export const Route = createFileRoute('/_setup/setup')({
  beforeLoad: async ({ context: { queryClient }, location }) => {
    const firstStep = setupStep(location.pathname) === 1
    const status = await queryClient.ensureQueryData(statusQuery)
    if (status.needs_setup && firstStep) return
    await requireSession(queryClient, location.href)
    if (firstStep) throw redirect({ to: '/setup/github' })
  },
  // A slow step shows the stepper with the step's own pending UI.
  pendingComponent: () => (
    <SetupLayout>
      <ChildPending layout={Route.id} />
    </SetupLayout>
  ),
  pendingMs: 0,
  pendingMinMs: 0,
  component: () => (
    <SetupLayout>
      <Outlet />
    </SetupLayout>
  ),
})

function SetupLayout({ children }: { children: ReactNode }) {
  const step = useLocation({ select: (location) => setupStep(location.pathname) })
  return (
    <>
      <SetupStepper current={step} />
      <SetupColumn>{children}</SetupColumn>
    </>
  )
}
