import { createFileRoute } from '@tanstack/react-router'
import { callbackSearch } from '~/components/pages/github/callback'
import { GitHubSettings, GitHubSettingsLoading, prefetch } from '~/components/pages/settings/github-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/github')({
  validateSearch: callbackSearch,
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  head: () => pageHead('GitHub settings'),
  pendingComponent: GitHubSettingsLoading,
  component: () => <GitHubSettings search={Route.useSearch()} />,
})
