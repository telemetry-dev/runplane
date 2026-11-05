import { createFileRoute } from '@tanstack/react-router'
import { NotificationSettings, prefetch } from '~/components/pages/settings/notifications-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/notifications')({
  head: () => pageHead('Notification settings'),
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  component: NotificationSettings,
})
