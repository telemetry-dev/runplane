import { createFileRoute } from '@tanstack/react-router'
import { sessionQuery } from '~/api/queries'
import { auditQuery, membersQuery } from '~/components/pages/settings/members-api'
import { MemberSettings } from '~/components/pages/settings/members-page'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/members')({
  head: () => pageHead('Members settings'),
  // GET /api/users and /api/audit are admin-only; the page shows its own loading and error states.
  loader: ({ context: { queryClient } }) => {
    if (queryClient.getQueryData(sessionQuery.queryKey)?.role !== 'admin') return
    void queryClient.prefetchQuery(membersQuery)
    void queryClient.prefetchQuery(auditQuery(''))
  },
  component: MemberSettings,
})
