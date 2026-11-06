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
  const mirror = cache.mirror_enabled ? cache.mirror : undefined
  return {
    size: cache.size_bytes === 0 ? '0' : (cache.size_bytes / gb).toFixed(1),
    limit: `${Math.round(cache.max_bytes / gb)} GB`,
    hitRate: hitRate(cache.hit_rate),
    entries: formatCount(cache.repositories.reduce((sum, repo) => sum + repo.entries, 0)),
    mirrorSaved: mirror && mirror.hits > 0 ? formatBytes(mirror.hit_bytes) : null,
  }
}

/** A hit rate of the API as "92%", or null when the API has no data for it. */
export function hitRate(rate: number | null): string | null {
  return rate === null ? null : formatPercent(rate * 100)
}

/** "since runplane started 2h ago", or "… on Oct 2" a week or more later. The mirror counts start over at each start. */
export function mirrorSince(startedAt: string, now = Date.now()): string {
  const week = 7 * 86_400_000
  return now - Date.parse(startedAt) < week
    ? `since runplane started ${formatRelativeTime(startedAt, now)}`
    : `since runplane started on ${formatDate(startedAt)}`
}

export function pulls(count: number): string {
  return count === 1 ? '1 pull' : `${formatCount(count)} pulls`
}

const gb = 1024 ** 3
