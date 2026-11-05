import { createFileRoute } from '@tanstack/react-router'
import { imagesQuery, poolsQuery, sandboxesQuery } from '~/components/pages/runners/queries'
import { RunnersPage, RunnersSkeleton } from '~/components/pages/runners/runners-page'
import { serversQuery } from '~/components/pages/servers/queries'
import { settingsQuery } from '~/components/pages/settings/settings-api'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/runners')({
  head: () => pageHead('Runners'),
  // ?label= marks one label, for links such as the size advice of Job detail.
  validateSearch: (search: Record<string, unknown>): { label?: string } =>
    typeof search.label === 'string' ? { label: search.label } : {},
  loader: async ({ context: { queryClient } }) => {
    // The label dialog needs these; load them before it opens.
    void queryClient.prefetchQuery(serversQuery)
    void queryClient.prefetchQuery(imagesQuery)
    void queryClient.prefetchQuery(settingsQuery)
    await Promise.all([queryClient.ensureQueryData(poolsQuery), queryClient.ensureQueryData(sandboxesQuery())])
  },
  pendingComponent: RunnersSkeleton,
  pendingMs: 200,
  component: RunnersPage,
})
