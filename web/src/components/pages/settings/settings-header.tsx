import { useCapabilities } from '~/api/session'
import { PageHeader } from '~/components/ui/page-header'
import { SegmentedLinks } from '~/components/ui/segmented'

// Notifications and Members use admin-only APIs.
const sections = [
  { to: '/settings/general', label: 'General', admin: false },
  { to: '/settings/github', label: 'GitHub', admin: false },
  { to: '/settings/runners', label: 'Runners', admin: false },
  { to: '/settings/notifications', label: 'Notifications', admin: true },
  { to: '/settings/members', label: 'Members', admin: true },
] as const

/** The title and section links of the "Settings · GitHub" design; each section is a child route. */
export function SettingsHeader() {
  const { admin } = useCapabilities()
  return (
    <div className="flex flex-col gap-l">
      <PageHeader title="Settings" />
      <SegmentedLinks aria-label="Settings sections" links={sections.filter((section) => admin || !section.admin)} />
    </div>
  )
}
