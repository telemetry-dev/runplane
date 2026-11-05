import { createFileRoute } from '@tanstack/react-router'
import { overviewQuery } from '~/api/overview'
import { isRange, type Range } from '~/components/pages/overview/data'
import { OverviewPage } from '~/components/pages/overview/overview-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/')({
  // 24h is the default and stays out of the URL.
  validateSearch: (search: Record<string, unknown>): { range?: Range } => ({
    range: isRange(search.range) && search.range !== '24h' ? search.range : undefined,
  }),
  loaderDeps: ({ search }) => ({ range: search.range ?? '24h' }),
  // Starts the request early, also on link hover; the page shows its own loading state.
  loader: ({ context: { queryClient }, deps }) => {
    void queryClient.prefetchQuery(overviewQuery(deps.range))
  },
  head: () => pageHead('Overview'),
  component: OverviewRoute,
})

function OverviewRoute() {
  const { range = '24h' } = Route.useSearch()
  const navigate = Route.useNavigate()
  return (
    <OverviewPage
      range={range}
      onRangeChange={(next) => void navigate({ search: { range: next === '24h' ? undefined : next }, replace: true })}
    />
  )
}
