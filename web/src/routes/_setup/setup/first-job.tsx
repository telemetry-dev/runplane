import { createFileRoute } from '@tanstack/react-router'
import { FirstJobStep, FirstJobStepLoading, prefetch } from '~/components/pages/setup/first-job-step'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/setup/first-job')({
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  head: () => pageHead('First job'),
  pendingComponent: FirstJobStepLoading,
  component: FirstJobStep,
})
