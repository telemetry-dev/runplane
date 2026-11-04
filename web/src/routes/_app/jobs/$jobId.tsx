import { createFileRoute } from '@tanstack/react-router'
import { JobPage, JobPageSkeleton } from '~/components/pages/jobs/job-page'
import { jobQuery } from '~/components/pages/jobs/job-queries'
import { parseJobSearch } from '~/components/pages/jobs/job-steps'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/jobs/$jobId')({
  validateSearch: parseJobSearch,
  // A failed load stays in the query cache, and the page shows it with a retry.
  loader: ({ context: { queryClient }, params }) => queryClient.ensureQueryData(jobQuery(params.jobId)).catch(() => undefined),
  head: ({ loaderData }) => pageHead(loaderData?.job_name ?? 'Job'),
  pendingComponent: JobPageSkeleton,
  pendingMs: 200,
  component: JobPage,
})
