import { describe, expect, test } from 'vitest'
import { cacheStats, hitRate, mirrorSince, type CacheOverview, type CacheRepository } from './cache-format'

// GET /api/caches of the seeded instance, with mirror stats.
const web: CacheRepository = {
  repository: 'kestrel-hq/web',
  entries: 4,
  size_bytes: 2030846773,
  hit_rate: 0.9197080291970803,
  hits: 126,
  misses: 11,
  last_used_at: '2026-11-04T04:24:45.447Z',
}
const docs: CacheRepository = { ...web, repository: 'kestrel-hq/docs', entries: 1, size_bytes: 134231253, hit_rate: null, hits: 0, misses: 0 }

const cache: CacheOverview = {
  enabled: true,
  size_bytes: 41231686042,
  max_bytes: 53687091200,
  hit_rate: 0.9197080291970803,
  repositories: [web, docs],
  mirror_enabled: true,
  mirror: {
    hits: 4812,
    misses: 328,
    hit_bytes: 335007449088,
    miss_bytes: 2000000,
    hit_rate: 0.9361867704280155,
    size_bytes: 19542101196,
    started_at: '2026-11-01T03:00:00Z',
  },
}

describe('cacheStats', () => {
  test('the size in GB with one decimal over the limit', () => {
    expect(cacheStats(cache)).toMatchObject({ size: '38.4', limit: '50 GB', entries: '5', hitRate: '92%', mirrorSaved: '312 GB' })
  })

  test('an empty cache shows 0, and no hit rate before the first lookup', () => {
    const empty = { ...cache, size_bytes: 0, hit_rate: null, repositories: [{ ...docs, entries: 0 }] }
    expect(cacheStats(empty)).toMatchObject({ size: '0', hitRate: null, entries: '0' })
  })

  test('no mirror savings while the mirror is off, has no stats, or has served nothing', () => {
    expect(cacheStats({ ...cache, mirror_enabled: false }).mirrorSaved).toBeNull()
    expect(cacheStats({ ...cache, mirror: undefined }).mirrorSaved).toBeNull()
    expect(cacheStats({ ...cache, mirror: { ...cache.mirror!, hits: 0, hit_bytes: 0 } }).mirrorSaved).toBeNull()
  })
})

test('a hit rate without lookups is no rate, not 0%', () => {
  expect(hitRate(docs.hit_rate)).toBeNull()
  expect(hitRate(web.hit_rate)).toBe('92%')
  expect(hitRate(0)).toBe('0%')
})

test.each([
  ['2026-11-04T07:59:58Z', 'since runplane started just now'],
  ['2026-11-04T05:30:00Z', 'since runplane started 2h ago'],
  ['2026-10-30T08:00:00Z', 'since runplane started 5d ago'],
  ['2026-10-02T08:00:00Z', 'since runplane started on Oct 2, 2026'],
])('mirrorSince(%s) is %j', (startedAt, want) => {
  expect(mirrorSince(startedAt, Date.parse('2026-11-04T08:00:00Z'))).toBe(want)
})
