import { createFileRoute } from '@tanstack/react-router'
import { JobsPage, prefetch } from '~/components/pages/jobs/jobs-page'
import { parseJobsSearch } from '~/components/pages/jobs/jobs-search'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/jobs/')({
  validateSearch: parseJobsSearch,
  loaderDeps: ({ search }) => search,
  loader: ({ context: { queryClient }, deps }) => prefetch(queryClient, deps),
  head: () => pageHead('Jobs'),
  component: () => <JobsPage search={Route.useSearch()} />,
})
