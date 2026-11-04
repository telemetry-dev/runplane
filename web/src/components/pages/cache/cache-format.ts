import type { Schemas } from '~/api/client'
import { formatBytes, formatCount, formatDate, formatPercent, formatRelativeTime } from '~/lib/format'

// Figures of the Cache design, from GET /api/caches.

export type CacheOverview = Schemas['CacheOverview']
export type CacheRepository = Schemas['CacheRepository']

type Stats = {
  /** "38.4", the size in GB with one decimal; "0" when empty. */
  size: string
  /** "50 GB", the cache_max_gb setting. */
  limit: string
  /** null before the cache had a hit or a miss in the last 7 days. */
  hitRate: string | null
  entries: string
  /** null while the mirror is off, has no stats, or served nothing yet. */
  mirrorSaved: string | null
}

export function cacheStats(cache: CacheOverview): Stats {
  const lookups = cache.repositories.reduce((sum, repo) => sum + repo.hits + repo.misses, 0)
  const mirror = cache.mirror_enabled ? cache.mirror : undefined
  return {
    size: cache.size_bytes === 0 ? '0' : (cache.size_bytes / gb).toFixed(1),
    limit: `${Math.round(cache.max_bytes / gb)} GB`,
    hitRate: lookups === 0 ? null : formatPercent(cache.hit_rate * 100),
    entries: formatCount(cache.repositories.reduce((sum, repo) => sum + repo.entries, 0)),
    mirrorSaved: mirror && mirror.hits > 0 ? formatBytes(mirror.hit_bytes) : null,
  }
}

/** The hit rate of a repository, or null without hits and misses. */
export function repositoryHitRate(repo: CacheRepository): string | null {
  return repo.hits + repo.misses === 0 ? null : formatPercent(repo.hit_rate * 100)
}

/** The mirror's hit rate since runplane started, or null before its first pull. */
export function mirrorHitRate(mirror: Schemas['MirrorStats']): string | null {
  const pulls = mirror.hits + mirror.misses
  return pulls === 0 ? null : formatPercent((mirror.hits / pulls) * 100)
}

/** "since runplane started 2h ago", or "… on Oct 2" a week or more later. The mirror counts start over at each start. */
export function mirrorSince(startedAt: string, now = Date.now()): string {
  const ago = formatRelativeTime(startedAt, now)
  if (ago === 'now') return 'since runplane started just now'
  const week = 7 * 86_400_000
  return now - Date.parse(startedAt) < week ? `since runplane started ${ago}` : `since runplane started on ${formatDate(startedAt)}`
}

export function pulls(count: number): string {
  return count === 1 ? '1 pull' : `${formatCount(count)} pulls`
}

const gb = 1024 ** 3
