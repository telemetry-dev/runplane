import { useSuspenseQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { serversQuery } from '~/api/servers'
import { useIsAdmin } from '~/api/session'
import { AddServerDialog } from '~/components/pages/servers/add-server-dialog'
import { ServersTable } from '~/components/pages/servers/servers-table'
import { PageHeader } from '~/components/ui/page-header'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/servers/')({
  head: () => pageHead('Servers'),
  loader: ({ context: { queryClient } }) => queryClient.ensureQueryData(serversQuery),
  pendingComponent: ServersLoading,
  component: ServersPage,
})

function ServersPage() {
  const { data: servers } = useSuspenseQuery(serversQuery)
  const admin = useIsAdmin()
  return (
    <>
      <PageHeader title="Servers" count={servers.length} actions={admin && <AddServerDialog />} />
      <ServersTable servers={servers} admin={admin} />
    </>
  )
}

function ServersLoading() {
  return (
    <>
      <PageHeader title="Servers" />
      <ServersTable servers={undefined} admin={false} />
    </>
  )
}
