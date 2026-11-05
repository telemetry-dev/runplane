import { createFileRoute } from '@tanstack/react-router'
import { callbackSearch } from '~/components/pages/github/callback'
import { GitHubStep, GitHubStepLoading, prefetch } from '~/components/pages/setup/github-step'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/setup/github')({
  validateSearch: callbackSearch,
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  head: () => pageHead('Connect GitHub'),
  pendingComponent: GitHubStepLoading,
  component: () => <GitHubStep search={Route.useSearch()} />,
})
