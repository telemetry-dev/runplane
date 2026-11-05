import { createFileRoute } from '@tanstack/react-router'
import { facetsQuery, jobsQuery } from '~/api/jobs'
import { JobsPage } from '~/components/pages/jobs/jobs-page'
import { jobsApiQuery, parseJobsSearch } from '~/components/pages/jobs/jobs-search'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/jobs/')({
  validateSearch: parseJobsSearch,
  loaderDeps: ({ search }) => search,
  // Warms the cache on link hover; the page shows its own loading state.
  loader: ({ context: { queryClient }, deps }) => {
    void queryClient.prefetchQuery(jobsQuery(jobsApiQuery(deps)))
    void queryClient.prefetchQuery(facetsQuery)
  },
  head: () => pageHead('Jobs'),
  component: JobsPage,
})
