import { createFileRoute } from '@tanstack/react-router'
import { prefetch, ServerPage, ServerPageSkeleton } from '~/components/pages/server-detail/server-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/servers/$serverId')({
  loader: ({ context: { queryClient }, params }) => prefetch(queryClient, params.serverId),
  head: ({ loaderData }) => pageHead(loaderData?.name ?? 'Server'),
  pendingComponent: ServerPageSkeleton,
  pendingMs: 200,
  component: () => <ServerPage serverId={Route.useParams().serverId} />,
})
