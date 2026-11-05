import { createFileRoute } from '@tanstack/react-router'
import { prefetch, RunnersPage, RunnersSkeleton } from '~/components/pages/runners/runners-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/runners')({
  head: () => pageHead('Runners'),
  // ?label= marks one label, for links such as the size advice of Job detail.
  validateSearch: (search: Record<string, unknown>): { label?: string } =>
    typeof search.label === 'string' ? { label: search.label } : {},
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  pendingComponent: RunnersSkeleton,
  pendingMs: 200,
  component: () => <RunnersPage marked={Route.useSearch().label} />,
})
