import { hashKey, useQueryClient, type QueryClient, type QueryKey } from '@tanstack/react-query'
import { useEffect, useEffectEvent } from 'react'
import { backupsQuery } from './backups'
import { cachesQuery } from './caches'
import { apiUrl, type Schemas } from './client'
import { putGitHub } from './github'
import { dropImage, imagesQuery } from './images'
import { jobQuery, jobsQuery, metricsQuery } from './jobs'
import { overviewQuery, putOverviewHostStats } from './overview'
import { poolsQuery, putPoolStats } from './pools'
import { runnerVersionsQuery } from './runner-versions'
import { sandboxesQuery, sandboxQuery } from './sandboxes'
import type { paths } from './schema'
import { dropServer, putHostStats, putServer, serverQuery, serversQuery } from './servers'
import { statusQuery } from './session'
import { domainDNSQuery, settingsQuery } from './settings'

/** An event of GET /api/events: its name, and the API object of the name as data. */
type LiveEvent = paths['/api/events']['get']['responses'][200]['content']['text/event-stream'][number]
type LiveEventName = LiveEvent['event']
type LiveEventData<N extends LiveEventName> = Extract<LiveEvent, { event: N }>['data']

/**
 * What each event changes in the cache: each function writes the payload into
 * the cached queries that hold it, and returns the queries that it makes stale.
 */
const changes: { [N in LiveEventName]: (client: QueryClient, data: LiveEventData<N>) => QueryKey[] } = {
  'job.state': (client, job) => {
    // The event has the list fields; the detail adds steps, log state, and metrics.
    client.setQueryData<object>(jobQuery(job.id).queryKey, (old) => old && { ...old, ...job })
    // The status tells whether runplane has a job, which changes only once.
    const firstJob = client.getQueryData<Schemas['Status']>(statusQuery.queryKey)?.has_jobs === false
    return [
      jobQuery(job.id).queryKey,
      metricsQuery(job.id).queryKey,
      jobsQuery().queryKey,
      overviewQuery().queryKey,
      ...(firstJob ? [statusQuery.queryKey] : []),
    ]
  },
  'sandbox.state': (_client, sandbox) => [
    sandboxesQuery().queryKey,
    sandboxQuery(sandbox.id).queryKey,
    // The pools count their runners, and each server counts its busy sandboxes as running jobs.
    poolsQuery.queryKey,
    serversQuery.queryKey,
    serverQuery(sandbox.server_id).queryKey,
    // A job's detail carries the state and ssh command of its debug sandbox.
    ...(sandbox.job_id ? [jobQuery(sandbox.job_id).queryKey] : []),
  ],
  'pool.stats': (client, stats) => {
    putPoolStats(client, stats)
    return []
  },
  // An image counts the pools that use it.
  'pools.changed': () => [poolsQuery.queryKey, imagesQuery.queryKey],
  'server.state': (client, server) => (putServer(client, server) ? serverCounts() : [serversQuery.queryKey, ...serverCounts()]),
  // An open Server detail of the removed server gets 404 and shows that the server is gone.
  'server.removed': (client, { id }) => {
    dropServer(client, id)
    return [serverQuery(id).queryKey, ...serverCounts()]
  },
  'host.stats': (client, stats) => {
    putHostStats(client, stats)
    putOverviewHostStats(client, stats)
    return []
  },
  'runner.version': () => [runnerVersionsQuery.queryKey],
  'image.state': () => [imagesQuery.queryKey],
  'image.removed': (client, { id }) => {
    dropImage(client, id)
    return []
  },
  'github.state': (client, status) => {
    putGitHub(client, status)
    return []
  },
  'backup.state': () => [backupsQuery.queryKey],
  'settings.changed': (client, settings) => {
    client.setQueryData(settingsQuery.queryKey, settings)
    // The status has the name and the mode, the DNS check the domain, the
    // caches their switches and size limit, and the pools their placement
    // after the host reserve and the CPU overcommit.
    return [statusQuery.queryKey, domainDNSQuery.queryKey, cachesQuery.queryKey, poolsQuery.queryKey]
  },
}

/** The queries that count servers: the overview, and the pools by the servers that can place their runners. */
function serverCounts(): QueryKey[] {
  return [overviewQuery().queryKey, poolsQuery.queryKey]
}

const eventNames = Object.keys(changes) as LiveEventName[]

/** Writes an event into the cache and returns the queries that it makes stale. */
function applyEvent<N extends LiveEventName>(client: QueryClient, name: N, data: LiveEventData<N>): QueryKey[] {
  return changes[name](client, data)
}

/** Changes the cache as an event would, for a mutation whose answer is the object of the event. */
export function applyChange<N extends LiveEventName>(client: QueryClient, name: N, data: LiveEventData<N>) {
  return refetch(client, applyEvent(client, name, data))
}

/** Refetches the server list and the queries that count servers, as for a server that the list does not have yet. */
export function refreshServers(client: QueryClient) {
  return refetch(client, [serversQuery.queryKey, ...serverCounts()])
}

function refetch(client: QueryClient, keys: QueryKey[]) {
  return Promise.all(keys.map((queryKey) => client.invalidateQueries({ queryKey })))
}

/**
 * Returns a function that applies one event to the cache. The queries that the
 * events make stale are refetched once per 300ms, so a burst of events causes
 * one request each.
 */
export function createEventHandler(client: QueryClient) {
  const stale = new Map<string, QueryKey>()
  let timer: ReturnType<typeof setTimeout> | undefined

  return (name: LiveEventName, data: unknown) => {
    for (const key of applyEvent(client, name, data as LiveEventData<typeof name>)) stale.set(hashKey(key), key)
    if (timer || stale.size === 0) return
    timer = setTimeout(() => {
      timer = undefined
      void refetch(client, [...stale.values()])
      stale.clear()
    }, 300)
  }
}

/** Delay before reconnect attempt `failures + 1`: 1s, 2s, 4s, and so on, at most 30s. */
export function reconnectDelay(failures: number): number {
  return Math.min(30_000, 1_000 * 2 ** failures)
}

/**
 * Keeps the one EventSource of the app open while the component is mounted,
 * so the events keep the cache current. `onOpen` runs each time the stream
 * opens: it sends no past events.
 */
export function useLiveEvents({ onOpen }: { onOpen?: () => void } = {}) {
  const client = useQueryClient()
  const opened = useEffectEvent(() => onOpen?.())
  useEffect(() => listen(client, () => opened()), [client])
}

type Stream = { listeners: Set<() => void>; close: () => void }

const streams = new WeakMap<QueryClient, Stream>()

/** Adds an open listener to the stream of `client`, and opens the stream for the first one. Returns the function that removes it. */
function listen(client: QueryClient, onOpen: () => void): () => void {
  let stream = streams.get(client)
  if (!stream) {
    stream = openStream(client)
    streams.set(client, stream)
  }
  stream.listeners.add(onOpen)
  return () => {
    stream.listeners.delete(onOpen)
    if (stream.listeners.size > 0) return
    stream.close()
    streams.delete(client)
  }
}

/**
 * The event stream of one cache. After a dropped stream it reconnects with
 * backoff and refetches all queries, because events may have been missed in
 * the gap.
 */
function openStream(client: QueryClient): Stream {
  const handle = createEventHandler(client)
  const listeners = new Set<() => void>()
  let source: EventSource | undefined
  let retry: ReturnType<typeof setTimeout> | undefined
  let failures = 0
  let missed = false

  function connect() {
    source = new EventSource(apiUrl('/api/events'))
    source.onopen = () => {
      if (missed) void client.invalidateQueries()
      missed = false
      failures = 0
      for (const listener of listeners) listener()
    }
    source.onerror = () => {
      // Close to stop the browser's fixed-interval retry; ours backs off.
      disconnect()
      retry = setTimeout(connect, reconnectDelay(failures++))
    }
    for (const name of eventNames) {
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
  return {
    listeners,
    close: () => {
      removeEventListener('pagehide', disconnect)
      removeEventListener('pageshow', onPageShow)
      disconnect()
    },
  }
}
