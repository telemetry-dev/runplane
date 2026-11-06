import { createFileRoute } from '@tanstack/react-router'
import { prefetch, ServersLoading, ServersPage } from '~/components/pages/servers/servers-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/servers/')({
  head: () => pageHead('Servers'),
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  pendingComponent: ServersLoading,
  component: ServersPage,
})
