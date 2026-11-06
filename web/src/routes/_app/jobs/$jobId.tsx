import { createFileRoute } from '@tanstack/react-router'
import { JobPage, JobPageSkeleton, prefetch } from '~/components/pages/jobs/job-page'
import { parseJobSearch } from '~/components/pages/jobs/job-steps'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/jobs/$jobId')({
  validateSearch: parseJobSearch,
  loader: ({ context: { queryClient }, params }) => prefetch(queryClient, params.jobId),
  head: ({ loaderData }) => pageHead(loaderData?.job_name ?? 'Job'),
  pendingComponent: JobPageSkeleton,
  pendingMs: 200,
  component: () => <JobPage jobId={Route.useParams().jobId} step={Route.useSearch().step} />,
})
