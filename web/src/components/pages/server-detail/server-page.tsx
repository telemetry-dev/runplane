import { useQuery, type QueryClient } from '@tanstack/react-query'
import { isApiError } from '~/api/client'
import { imagesQuery } from '~/api/images'
import { sandboxesQuery } from '~/api/sandboxes'
import { doctorQuery, serverQuery, useDoctor } from '~/api/servers'
import { useCapabilities } from '~/api/session'
import { LoadError, LoadErrorRow, NotFound } from '~/components/app/load-error'
import { SectionHeader } from '~/components/app/section-header'
import { serverStatusNames } from '~/components/app/status-dot'
import { LiveRunnersTable } from '~/components/pages/runners/live-runners-table'
import { statusFacts } from '~/components/pages/servers/server-info'
import { Skeleton } from '~/components/ui/skeleton'
import { DoctorSection } from './doctor-section'
import { ImagesTable } from './images-table'
import { ServerFacts, ServerUsage } from './server-facts'
import type { Server } from './server-format'
import { ServerHeader } from './server-header'

/** Starts the requests of the page, and returns the server for the title. */
export function prefetch(client: QueryClient, serverId: string) {
  void client.prefetchQuery(sandboxesQuery(serverId))
  void client.prefetchQuery(imagesQuery)
  void client.prefetchQuery(doctorQuery(serverId))
  // A failed load stays in the query cache, and the page shows it with a retry.
  return client.ensureQueryData(serverQuery(serverId)).catch(() => undefined)
}

/** The Server detail design: facts and usage of one server, the doctor checks, its runners, and its images. */
export function ServerPage({ serverId }: { serverId: string }) {
  const query = useQuery(serverQuery(serverId))

  if (query.isPending) return <ServerPageSkeleton />
  // A removed server answers 404 and replaces the page; other failed refetches keep the last data.
  if (isApiError(query.error, 404)) {
    return (
      <NotFound title="Server not found" to="/servers" link="All servers">
        No server has this ID. It may have been removed.
      </NotFound>
    )
  }
  if (!query.data) {
    return <LoadError heading={1} what="the server" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} />
  }
  return <ServerDetail server={query.data} />
}

function ServerDetail({ server }: { server: Server }) {
  const { admin } = useCapabilities()
  const sandboxes = useQuery(sandboxesQuery(server.id))
  const images = useQuery(imagesQuery)
  const doctor = useDoctor(server.id)

  return (
    <>
      <ServerHeader server={server} admin={admin} doctor={doctor} />
      <ServerFacts server={server} />
      <ServerUsage server={server} />
      <DoctorSection server={server} admin={admin} doctor={doctor} />
      <section className="flex flex-col gap-s">
        <SectionHeader title="Runners on this server" count={sandboxes.data?.length || undefined} />
        <LiveRunnersTable
          sandboxes={sandboxes.data}
          showServer={false}
          empty={
            statusFacts[server.status].connected && !statusFacts[server.status].takesRunners
              ? `No runners on this server. ${serverStatusNames[server.status]} servers take no new runners.`
              : 'No runners on this server.'
          }
          failed={
            sandboxes.isError ? (
              <LoadErrorRow what="the runners" error={sandboxes.error} onRetry={() => void sandboxes.refetch()} retrying={sandboxes.isFetching} />
            ) : undefined
          }
        />
      </section>
      <section className="flex flex-col gap-s">
        <SectionHeader title="Images" count={images.data?.length || undefined} />
        <ImagesTable
          server={server}
          images={images.data}
          failed={
            images.isError ? (
              <LoadErrorRow what="the images" error={images.error} onRetry={() => void images.refetch()} retrying={images.isFetching} />
            ) : undefined
          }
        />
      </section>
    </>
  )
}

/** The page while the server loads: the shapes of the header, facts, and usage. */
export function ServerPageSkeleton() {
  return (
    <div aria-busy className="flex flex-col gap-xxl">
      <p className="sr-only" role="status">
        Loading the server
      </p>
      <div className="flex flex-col gap-s" aria-hidden>
        <Skeleton className="my-[3px] h-[12px] w-[160px]" />
        <Skeleton className="my-[3px] h-[24px] w-[220px]" />
      </div>
      <div className="-mt-m flex flex-wrap gap-xxl" aria-hidden>
        {[96, 112, 96, 128, 120, 140].map((width, index) => (
          <span key={index} className="flex flex-col gap-xs">
            <Skeleton className="h-[10px]" style={{ width: width / 2 }} />
            <Skeleton className="h-[14px]" style={{ width }} />
          </span>
        ))}
      </div>
      <span aria-hidden className="block h-[86px] animate-pulse rounded-m bg-neutral-2" />
      <span aria-hidden className="block h-[240px] animate-pulse rounded-m bg-neutral-2" />
    </div>
  )
}
