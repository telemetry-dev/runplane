import { createFileRoute } from '@tanstack/react-router'
import { GeneralSettings } from '~/components/pages/settings/general-page'
import { settingsQuery } from '~/components/pages/settings/settings-api'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/general')({
  head: () => pageHead('General settings'),
  // Sections show their own loading and error states, so the loader only starts the request.
  loader: ({ context: { queryClient } }) => void queryClient.prefetchQuery(settingsQuery),
  component: GeneralSettings,
})
