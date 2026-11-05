import { createFileRoute } from '@tanstack/react-router'
import { imagesQuery } from '~/api/images'
import { sandboxesQuery } from '~/api/sandboxes'
import { doctorQuery, serverQuery } from '~/api/servers'
import { ServerPage, ServerPageSkeleton } from '~/components/pages/server-detail/server-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/servers/$serverId')({
  loader: async ({ context: { queryClient }, params }) => {
    void queryClient.prefetchQuery(sandboxesQuery(params.serverId))
    void queryClient.prefetchQuery(imagesQuery)
    void queryClient.prefetchQuery(doctorQuery(params.serverId))
    // A failed load stays in the query cache, and the page shows it with a retry.
    return queryClient.ensureQueryData(serverQuery(params.serverId)).catch(() => undefined)
  },
  head: ({ loaderData }) => pageHead(loaderData?.name ?? 'Server'),
  pendingComponent: ServerPageSkeleton,
  pendingMs: 200,
  component: ServerPage,
})
