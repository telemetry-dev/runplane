import { useSuspenseQuery, type QueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { cachesQuery } from '~/api/caches'
import { useCapabilities } from '~/api/session'
import { KeyValue, KeyValueList } from '~/components/app/key-value'
import { None } from '~/components/app/none'
import { SectionHeader } from '~/components/app/section-header'
import { Stat } from '~/components/app/stat'
import { PageHeader } from '~/components/ui/page-header'
import { formatBytes, formatDateTime } from '~/lib/format'
import { cacheStats, hitRate, mirrorSince, pulls, type CacheOverview } from './cache-format'
import { RepositoriesTable } from './repositories-table'

export function prefetch(client: QueryClient) {
  return client.ensureQueryData(cachesQuery)
}

/** The Cache design: the Actions cache by repository, and the registry mirror. */
export function CachePage() {
  const { data: cache } = useSuspenseQuery({ ...cachesQuery, refetchInterval })
  const { admin } = useCapabilities()
  const stats = cacheStats(cache)
  const repositories = cache.enabled ? cache.repositories.length : 0

  return (
    <>
      <PageHeader title="Cache" />
      <StatsRow>
        <Stat label="Size" value={stats.size} total={stats.limit} />
        <Stat label="Hit rate, last 7 days" value={stats.hitRate} />
        <Stat label="Entries" value={stats.entries} />
        <Stat label="Downloads saved by the mirror" value={stats.mirrorSaved} />
      </StatsRow>
      <section className="flex flex-col gap-s">
        <SectionHeader title="Repositories" count={repositories || undefined} />
        <RepositoriesTable cache={cache} admin={admin} />
      </section>
      <RegistryMirror cache={cache} />
    </>
  )
}

// No event reports cache use, so the page refetches it while it is open.
const refetchInterval = 30_000

/** The page while the overview loads. */
export function CacheSkeleton() {
  return (
    <>
      <PageHeader title="Cache" />
      <StatsRow>
        <Stat label="Size" value={null} />
        <Stat label="Hit rate, last 7 days" value={null} />
        <Stat label="Entries" value={null} />
        <Stat label="Downloads saved by the mirror" value={null} />
      </StatsRow>
      <section className="flex flex-col gap-s">
        <SectionHeader title="Repositories" />
        <RepositoriesTable cache={undefined} admin={false} />
      </section>
    </>
  )
}

// Four equal columns; two per row in a narrow page.
function StatsRow({ children }: { children: ReactNode }) {
  return (
    <div className="@container">
      <div className="grid grid-cols-2 gap-xl @2xl:grid-cols-4">{children}</div>
    </div>
  )
}

/** The counts of the Docker Hub pull-through cache, which start over when runplane starts. */
function RegistryMirror({ cache }: { cache: CacheOverview }) {
  const mirror = cache.mirror
  return (
    <section className="flex flex-col gap-m">
      <h2 className="flex min-h-[22px] flex-wrap items-baseline gap-x-xs text-m font-heavy tracking-l text-neutral-10">
        Registry mirror
        {cache.mirror_enabled && mirror && (
          <span className="font-regular text-neutral-6" title={formatDateTime(mirror.started_at, { full: true })}>
            {mirrorSince(mirror.started_at)}
          </span>
        )}
      </h2>
      {!cache.mirror_enabled ? (
        <p className="text-s text-neutral-7">
          The registry mirror is off. Turn it on in{' '}
          <Link to="/settings/runners" className="font-medium text-neutral-10 underline underline-offset-2">
            Settings › Runners
          </Link>
          .
        </p>
      ) : (
        <KeyValueList>
          <KeyValue label="Stored">{mirror ? formatBytes(mirror.size_bytes) : <None />}</KeyValue>
          <KeyValue label="Hits">{mirror ? pulls(mirror.hits) : <None />}</KeyValue>
          <KeyValue label="Misses">{mirror ? pulls(mirror.misses) : <None />}</KeyValue>
          <KeyValue label="Hit rate">{(mirror && hitRate(mirror.hit_rate)) ?? <None />}</KeyValue>
        </KeyValueList>
      )}
    </section>
  )
}
