import { useQuery } from '@tanstack/react-query'
import { Link, useParams } from '@tanstack/react-router'
import { errorMessage, isApiError } from '~/api/client'
import { imagesQuery } from '~/api/images'
import { sandboxesQuery } from '~/api/sandboxes'
import { serverQuery, useDoctor } from '~/api/servers'
import { useIsAdmin } from '~/api/session'
import { SectionHeader } from '~/components/app/section-header'
import { TextButton } from '~/components/app/text-button'
import { LiveRunnersTable } from '~/components/pages/runners/live-runners-table'
import { Button, buttonClassName } from '~/components/ui/button'
import { Skeleton } from '~/components/ui/skeleton'
import { DoctorSection } from './doctor-section'
import { ImagesTable } from './images-table'
import { ServerFacts, ServerUsage } from './server-facts'
import type { Server } from './server-format'
import { ServerHeader } from './server-header'

/** The Server detail design: facts and usage of one server, the doctor checks, its runners, and its images. */
export function ServerPage() {
  const { serverId } = useParams({ from: '/_app/servers/$serverId' })
  const query = useQuery(serverQuery(serverId))

  if (query.isPending) return <ServerPageSkeleton />
  // A removed server answers 404 and replaces the page; other failed refetches keep the last data.
  if (!query.data || isApiError(query.error, 404)) return <ServerError error={query.error} onRetry={() => void query.refetch()} />
  return <ServerDetail server={query.data} />
}

function ServerDetail({ server }: { server: Server }) {
  const admin = useIsAdmin()
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
            server.status === 'draining'
              ? 'No runners on this server. This server takes no new runners while it drains.'
              : 'No runners on this server.'
          }
          failed={sandboxes.isError && <LoadFailed what="runners" error={sandboxes.error} onRetry={() => void sandboxes.refetch()} />}
        />
      </section>
      <section className="flex flex-col gap-s">
        <SectionHeader title="Images" count={images.data?.length || undefined} />
        <ImagesTable
          server={server}
          images={images.data}
          failed={images.isError && <LoadFailed what="images" error={images.error} onRetry={() => void images.refetch()} />}
        />
      </section>
    </>
  )
}

/** The text of a table whose list did not load, once the retries gave up. */
function LoadFailed({ what, error, onRetry }: { what: string; error: unknown; onRetry: () => void }) {
  return (
    <span role="alert">
      Could not load the {what}. {errorMessage(error)} <TextButton onClick={onRetry}>Try again</TextButton>
    </span>
  )
}

function ServerError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const missing = isApiError(error, 404)
  return (
    <div role="alert" className="flex flex-col items-start gap-m">
      <h1 className="type-page-title text-neutral-10">{missing ? 'Server not found' : 'Could not load the server'}</h1>
      <p className="text-s text-neutral-8">{missing ? 'No server has this ID. It may have been removed.' : errorMessage(error)}</p>
      {missing ? (
        <Link to="/servers" className={buttonClassName('secondary')}>
          All servers
        </Link>
      ) : (
        <Button onClick={onRetry}>Try again</Button>
      )}
    </div>
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
