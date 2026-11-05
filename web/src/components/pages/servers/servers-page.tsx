import { useSuspenseQuery, type QueryClient } from '@tanstack/react-query'
import { serversQuery } from '~/api/servers'
import { useIsAdmin } from '~/api/session'
import { PageHeader } from '~/components/ui/page-header'
import { AddServerDialog } from './add-server-dialog'
import { ServersTable } from './servers-table'

export function prefetch(client: QueryClient) {
  return client.ensureQueryData(serversQuery)
}

/** The Servers design: every server with its state and use, and "Add server" for admins. */
export function ServersPage() {
  const { data: servers } = useSuspenseQuery(serversQuery)
  const admin = useIsAdmin()
  return (
    <>
      <PageHeader title="Servers" count={servers.length} actions={admin && <AddServerDialog />} />
      <ServersTable servers={servers} admin={admin} />
    </>
  )
}

/** The page while the servers load. */
export function ServersLoading() {
  return (
    <>
      <PageHeader title="Servers" />
      <ServersTable servers={undefined} admin={false} />
    </>
  )
}
