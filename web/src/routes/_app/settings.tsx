import { createFileRoute, Outlet } from '@tanstack/react-router'
import { SettingsHeader } from '~/components/pages/settings/settings-header'
import { NestedRouteError } from '~/components/route-error'
import { ChildPending } from '~/components/shell/pending'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings')({
  head: () => pageHead('Settings'),
  component: () => (
    <>
      <SettingsHeader />
      <NestedRouteError value>
        <Outlet />
      </NestedRouteError>
    </>
  ),
  // The header needs no data, so it shows at once and the section's own pending UI follows.
  pendingComponent: () => (
    <>
      <SettingsHeader />
      <ChildPending layout={Route.id} />
    </>
  ),
  pendingMs: 0,
  pendingMinMs: 0,
})
