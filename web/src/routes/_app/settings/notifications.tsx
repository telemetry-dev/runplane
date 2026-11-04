import { createFileRoute } from '@tanstack/react-router'
import { sessionQuery } from '~/api/queries'
import { notificationsQuery } from '~/components/pages/settings/notifications-api'
import { NotificationSettings } from '~/components/pages/settings/notifications-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/notifications')({
  head: () => pageHead('Notification settings'),
  // GET /api/notifications is admin-only; the page shows its own loading and error states.
  loader: ({ context: { queryClient } }) => {
    if (queryClient.getQueryData(sessionQuery.queryKey)?.role === 'admin') void queryClient.prefetchQuery(notificationsQuery)
  },
  component: NotificationSettings,
})
