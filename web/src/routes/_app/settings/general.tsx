import { createFileRoute } from '@tanstack/react-router'
import { GeneralSettings, prefetch } from '~/components/pages/settings/general-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/general')({
  head: () => pageHead('General settings'),
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  component: GeneralSettings,
})
