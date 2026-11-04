import { hashKey, notifyManager, useQueryClient, type QueryFunction, type QueryKey, type SkipToken } from '@tanstack/react-query'
import { useMatches, useRouter } from '@tanstack/react-router'
import { createContext, useCallback, useContext, useEffect, useState, useSyncExternalStore } from 'react'

// On a direct page load the router shows the pendingComponent of the first
// layout route (the one below the root) until every loader of the page is
// done, and the pages' own pendingComponents never show. So the layouts use
// these in their pendingComponent: their frame, with the page's pending UI in it.

/** Time before a layout shows its frame, so a fast load goes from blank straight to the page. */
export const frameDelayMs = 150

/**
 * The pendingComponent of the route below `layout` (a route ID), after that
 * route's pendingMs, as the router would show it on a client-side navigation.
 * A nested layout shows its pages' pending UI only if it has a pendingComponent
 * of its own that renders its frame and ChildPending again, as Settings does.
 */
export function ChildPending({ layout }: { layout: string }) {
  const router = useRouter()
  const childId = useMatches({ select: (matches) => matches[matches.findIndex((match) => match.routeId === layout) + 1]?.routeId })
  const route = childId === undefined ? undefined : router.routesById[childId]
  const Pending = route?.options.pendingComponent
  const shown = useElapsed(route?.options.pendingMs ?? router.options.defaultPendingMs ?? 1000)
  return shown && Pending ? <Pending /> : null
}

/** False until `ms` have passed since the first render. */
export function useElapsed(ms: number): boolean {
  const [elapsed, setElapsed] = useState(ms <= 0)
  useEffect(() => {
    if (elapsed) return
    const timer = setTimeout(() => setElapsed(true), ms)
    return () => clearTimeout(timer)
  }, [elapsed, ms])
  return elapsed
}

const PendingFrameContext = createContext(false)

/** Wraps the frame that a layout's pendingComponent shows: `<PendingFrame value>…</PendingFrame>`. */
export const PendingFrame = PendingFrameContext.Provider

/**
 * The `subscribed` option for queries of the layout frame. A pending frame only
 * reads the cache: in dev, StrictMode unmounts each new component once, and
 * when the last observer of a query leaves, TanStack Query cancels its fetch,
 * which fails a route loader that awaits it.
 */
export function useFrameSubscribed(): boolean {
  return !useContext(PendingFrameContext)
}

/** Data of a query in the cache, kept current without fetching it or adding an observer. */
export function useCachedData<T>(query: { queryKey: QueryKey; queryFn?: QueryFunction<T, never> | SkipToken }): T | undefined {
  const client = useQueryClient()
  const hash = hashKey(query.queryKey)
  const subscribe = useCallback(
    (onChange: () => void) => {
      // The cache also changes while other components render (a new useQuery
      // adds its query), and React refuses an update during another's render.
      // So listen to this query only, and tell React after the render, as useQuery does.
      const notify = notifyManager.batchCalls(onChange)
      return client.getQueryCache().subscribe((event) => {
        if (event.query.queryHash === hash) notify()
      })
    },
    [client, hash],
  )
  const read = () => client.getQueryData<T>(query.queryKey)
  // The static SPA shell renders this too, with an empty cache.
  return useSyncExternalStore(subscribe, read, read)
}
