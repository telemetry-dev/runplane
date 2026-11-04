import { createFileRoute } from '@tanstack/react-router'
import { RunnerSettings } from '~/components/pages/settings/runners-page'
import { settingsQuery } from '~/components/pages/settings/settings-api'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/runners')({
  head: () => pageHead('Runner settings'),
  // Sections show their own loading and error states, so the loader only starts the request.
  loader: ({ context: { queryClient } }) => void queryClient.prefetchQuery(settingsQuery),
  component: RunnerSettings,
})
