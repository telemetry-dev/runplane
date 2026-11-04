import { createFileRoute, Outlet, redirect, useLocation } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { sessionQuery, statusQuery } from '~/api/queries'
import { ChildPending } from '~/components/shell/pending'
import { SetupColumn, SetupStepper } from '~/components/shell/setup'

// Step 1 is open only until the first admin exists; steps 2 and 3 need that admin's session.
export const Route = createFileRoute('/_setup/setup')({
  beforeLoad: async ({ context: { queryClient }, location }) => {
    const status = await queryClient.ensureQueryData(statusQuery)
    const firstStep = stepOf(location.pathname) === 1
    if (status.needs_setup) {
      if (!firstStep) throw redirect({ to: '/setup' })
      return
    }
    const user = await queryClient.ensureQueryData(sessionQuery)
    if (!user) throw redirect({ to: '/login', search: { redirect: location.href } })
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
  const step = useLocation({ select: (location) => stepOf(location.pathname) })
  return (
    <>
      <SetupStepper current={step} />
      <SetupColumn>{children}</SetupColumn>
    </>
  )
}

function stepOf(pathname: string): number {
  if (pathname.startsWith('/setup/first-job')) return 3
  if (pathname.startsWith('/setup/github')) return 2
  return 1
}
