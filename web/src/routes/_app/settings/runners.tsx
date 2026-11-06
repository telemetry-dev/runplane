import { createFileRoute } from '@tanstack/react-router'
import { RunnerSettings, prefetch } from '~/components/pages/settings/runners-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/runners')({
  head: () => pageHead('Runner settings'),
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  component: RunnerSettings,
})
