import { createFileRoute } from '@tanstack/react-router'
import { MemberSettings, prefetch } from '~/components/pages/settings/members-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/members')({
  head: () => pageHead('Members settings'),
  loader: ({ context: { queryClient } }) => prefetch(queryClient),
  component: MemberSettings,
})
