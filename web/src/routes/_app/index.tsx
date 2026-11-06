import { createFileRoute } from '@tanstack/react-router'
import { parseOverviewSearch } from '~/components/pages/overview/data'
import { OverviewPage, prefetch } from '~/components/pages/overview/overview-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/')({
  validateSearch: parseOverviewSearch,
  loaderDeps: ({ search }) => search,
  loader: ({ context: { queryClient }, deps }) => prefetch(queryClient, deps.range),
  head: () => pageHead('Overview'),
  component: () => <OverviewPage range={Route.useSearch().range} />,
})
