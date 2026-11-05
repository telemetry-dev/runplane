import { createFileRoute } from '@tanstack/react-router'
import { InvitePage } from '~/components/pages/setup/invite-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/invite/$token')({
  head: () => pageHead('Join'),
  component: () => <InvitePage token={Route.useParams().token} />,
})
