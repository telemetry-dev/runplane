import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Outlet } from '@tanstack/react-router'
import { sessionQuery } from '~/api/queries'
import { NestedRouteError } from '~/components/route-error'
import { ChildPending } from '~/components/shell/pending'
import { PageHeader } from '~/components/ui/page-header'
import { SegmentedLinks } from '~/components/ui/segmented'
import { pageHead } from '~/lib/head'

// Title and section links of the "Settings · GitHub" design; each section is a child route.
export const Route = createFileRoute('/_app/settings')({
  head: () => pageHead('Settings'),
  component: SettingsLayout,
  // The header needs no data, so it shows at once and the section's own pending UI follows.
  pendingComponent: SettingsPending,
  pendingMs: 0,
  pendingMinMs: 0,
})

function SettingsLayout() {
  return (
    <>
      <SettingsHeader />
      <NestedRouteError value>
        <Outlet />
      </NestedRouteError>
    </>
  )
}

function SettingsPending() {
  return (
    <>
      <SettingsHeader />
      <ChildPending layout={Route.id} />
    </>
  )
}

// Notifications and Members use admin-only APIs.
const sections = [
  { to: '/settings/general', label: 'General', admin: false },
  { to: '/settings/github', label: 'GitHub', admin: false },
  { to: '/settings/runners', label: 'Runners', admin: false },
  { to: '/settings/notifications', label: 'Notifications', admin: true },
  { to: '/settings/members', label: 'Members', admin: true },
] as const

function SettingsHeader() {
  const admin = useQuery(sessionQuery).data?.role === 'admin'
  return (
    <div className="flex flex-col gap-l">
      <PageHeader title="Settings" />
      <SegmentedLinks aria-label="Settings sections" links={sections.filter((section) => admin || !section.admin)} />
    </div>
  )
}
