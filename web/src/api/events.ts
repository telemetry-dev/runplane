import { skipToken, useQuery, useQueryClient, type QueryClient, type QueryKey } from '@tanstack/react-query'
import { useEffect } from 'react'
import type { Schemas } from './client'

/** SSE event names of GET /api/events. */
const liveEvents = [
  'sandbox.state',
  'job.state',
  'pool.stats',
  'server.state',
  'host.stats',
  'runner.version',
  'image.state',
  'github.state',
  'backup.state',
] as const

export type LiveEvent = (typeof liveEvents)[number]

export type PoolStats = {
  pool_id: string
  idle: number
  busy: number
  starting: number
  queued: number
  assigned: number
}

export type HostStats = {
  server_id: string
  cpu_percent: number
  memory_percent: number
  disk_percent: number
  at: string
}

/**
 * Opens the one EventSource of the app and keeps the TanStack Query cache current.
 * After a dropped stream it reconnects with backoff and refetches all API queries,
 * because events may have been missed in the gap.
 */
export function useLiveEvents() {
  const client = useQueryClient()

  useEffect(() => {
    const handle = createEventHandler(client)
    let source: EventSource | undefined
    let retry: ReturnType<typeof setTimeout> | undefined
    let failures = 0
    let missed = false

    function connect() {
      source = new EventSource('/api/events')
      source.onopen = () => {
        if (missed) void client.invalidateQueries({ queryKey: ['get'] })
        missed = false
        failures = 0
      }
      source.onerror = () => {
        // Close to stop the browser's fixed-interval retry; ours backs off.
        disconnect()
        retry = setTimeout(connect, reconnectDelay(failures++))
      }
      for (const name of liveEvents) {
        source.addEventListener(name, (event) => handle(name, JSON.parse(event.data)))
      }
    }

    function disconnect() {
      clearTimeout(retry)
      source?.close()
      missed = true
    }

    // A page in the back/forward cache keeps its stream open otherwise, and
    // browsers allow only six connections to one host over HTTP/1.1.
    function onPageShow(event: PageTransitionEvent) {
      if (event.persisted) connect()
    }

    connect()
    addEventListener('pagehide', disconnect)
    addEventListener('pageshow', onPageShow)
    return () => {
      removeEventListener('pagehide', disconnect)
      removeEventListener('pageshow', onPageShow)
      disconnect()
    }
  }, [client])
}

/** The latest pool.stats event of a pool, or undefined until one arrives. */
export function usePoolStats(poolId: string): PoolStats | undefined {
  return useQuery<PoolStats>({ queryKey: liveKey('pool.stats', poolId), queryFn: skipToken }).data
}

/** The latest host.stats event of a server (each 10 seconds), or undefined until one arrives. */
export function useHostStats(serverId: string): HostStats | undefined {
  return useQuery<HostStats>({ queryKey: liveKey('host.stats', serverId), queryFn: skipToken }).data
}

/**
 * Returns a function that applies one event to the cache. Objects in the event
 * payload go straight into cached queries. Queries that the event makes stale
 * are refetched once per 300ms, so a burst of events causes one request each.
 */
export function createEventHandler(client: QueryClient) {
  const stale = new Map<string, Stale>()
  let timer: ReturnType<typeof setTimeout> | undefined

  return (name: LiveEvent, data: unknown) => {
    for (const target of applyEvent(client, name, data)) stale.set(`${target.path} ${target.id ?? ''}`, target)
    if (timer || stale.size === 0) return
    timer = setTimeout(() => {
      timer = undefined
      for (const { path, id } of stale.values()) {
        void client.invalidateQueries({
          queryKey: ['get', path],
          predicate: id === undefined ? undefined : (query) => pathId(query.queryKey) === id,
        })
      }
      stale.clear()
    }, 300)
  }
}

/** Delay before reconnect attempt `failures + 1`: 1s, 2s, 4s, and so on, at most 30s. */
export function reconnectDelay(failures: number): number {
  return Math.min(30_000, 1_000 * 2 ** failures)
}

/** The queries of an API path, or only those for one path `id`. */
type Stale = { path: string; id?: string }

/** Writes the event payload into the cache and returns the queries whose data it changes. */
function applyEvent(client: QueryClient, name: LiveEvent, data: unknown): Stale[] {
  switch (name) {
    case 'job.state': {
      // The event has the list fields; the detail adds steps, log state, and metrics.
      const { id } = data as Item
      mergeDetail(client, '/api/jobs/{id}', data as Item)
      return [{ path: '/api/jobs/{id}', id }, { path: '/api/jobs/{id}/metrics', id }, { path: '/api/jobs' }, { path: '/api/overview' }]
    }
    case 'sandbox.state': {
      // A job's detail carries the state and ssh command of its debug sandbox.
      // The server list counts the busy sandboxes of each server as its running jobs.
      const { id, job_id, server_id } = data as Sandbox
      const stale: Stale[] = [
        { path: '/api/sandboxes' },
        { path: '/api/sandboxes/{id}', id },
        { path: '/api/pools' },
        { path: '/api/servers' },
        { path: '/api/servers/{id}', id: server_id },
      ]
      if (job_id) stale.push({ path: '/api/jobs/{id}', id: job_id })
      return stale
    }
    case 'pool.stats':
      client.setQueryData(liveKey(name, (data as PoolStats).pool_id), data)
      return []
    case 'server.state': {
      // The event has the Server object of GET /api/servers and GET /api/servers/{id}.
      // The pools count the online servers that can place their runners.
      const server = data as Schemas['Server']
      mergeDetail(client, '/api/servers/{id}', server)
      const stale: Stale[] = [{ path: '/api/overview' }, { path: '/api/pools' }]
      if (!replaceInLists(client, '/api/servers', server)) stale.push({ path: '/api/servers' })
      return stale
    }
    case 'host.stats':
      client.setQueryData(liveKey(name, (data as HostStats).server_id), data)
      return []
    case 'runner.version':
      return [{ path: '/api/runner-versions' }]
    case 'image.state':
      return [{ path: '/api/images' }]
    case 'github.state':
      // The event carries the whole GET /api/github answer.
      client.setQueriesData({ queryKey: ['get', '/api/github'] }, data)
      return []
    case 'backup.state':
      return [{ path: '/api/backups' }]
  }
}

type Item = { id: string }

type Sandbox = Pick<Schemas['Sandbox'], 'id' | 'job_id' | 'server_id'>

// List payloads lack detail fields (job steps, for example), so merge instead of replace.
function mergeDetail(client: QueryClient, path: string, item: Item) {
  client.setQueriesData<object>(
    { queryKey: ['get', path], predicate: (query) => pathId(query.queryKey) === item.id },
    (old) => old && { ...old, ...item },
  )
}

/**
 * Puts the item in place of the item with its ID in each cached list of the path.
 * Returns false if a cached list lacks the item, as for a new server.
 */
function replaceInLists(client: QueryClient, path: string, item: Item): boolean {
  let found = true
  client.setQueriesData<Item[]>({ queryKey: ['get', path] }, (list) => {
    if (!list) return list
    const index = list.findIndex((listed) => listed.id === item.id)
    if (index < 0) {
      found = false
      return list
    }
    return list.with(index, item)
  })
  return found
}

function pathId(key: QueryKey): unknown {
  const init = key[2] as { params?: { path?: { id?: unknown } } } | undefined
  return init?.params?.path?.id
}

function liveKey(name: 'pool.stats' | 'host.stats', id: string): QueryKey {
  return ['live', name, id]
}
