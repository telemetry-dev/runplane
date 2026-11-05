import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createRouter } from '@tanstack/react-router'
import { auditQuery } from './api/audit'
import { isApiError } from './api/client'
import { sessionQuery } from './api/session'
import { RouteError } from './components/route-error'
import { withGallery } from './dev/gallery-route'
import { routeTree } from './routeTree.gen'

export function getRouter() {
  const queryClient: QueryClient = new QueryClient({
    queryCache: new QueryCache({ onError: signInAgain }),
    mutationCache: new MutationCache({
      onError: signInAgain,
      // Most changes add an entry to the audit log, which Settings · Members shows.
      onSuccess: () => queryClient.invalidateQueries({ queryKey: auditQuery().queryKey }),
    }),
    defaultOptions: {
      queries: {
        // Retry network and server failures, not answers like 404 or 403.
        retry: (failures, error) => failures < 2 && !(isApiError(error) && error.status < 500),
      },
    },
  })

  const router = createRouter({
    routeTree: import.meta.env.DEV ? withGallery(routeTree) : routeTree,
    context: { queryClient },
    defaultPreload: 'intent',
    // Loaders read through TanStack Query, which owns caching.
    defaultPreloadStaleTime: 0,
    defaultErrorComponent: RouteError,
    // SPA mode renders this in place of the routes in the static shell, so keep it empty.
    defaultPendingComponent: () => null,
    scrollRestoration: true,
    Wrap: ({ children }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>,
  })

  // A new page moves the focus to its heading, so keyboard and screen reader
  // users start there, not on the sidebar link. The first page of a visit, and
  // a change of the search only, as a Jobs filter, keep the focus.
  router.subscribe('onRendered', ({ pathChanged, fromLocation }) => {
    if (!pathChanged || !fromLocation || typeof document === 'undefined') return
    const target = document.querySelector<HTMLElement>('main h1') ?? document.querySelector<HTMLElement>('main')
    if (!target) return
    target.tabIndex = -1
    target.focus({ preventScroll: true })
  })

  // An expired session inside the app sends the user to the login page and back afterwards.
  function signInAgain(error: Error) {
    if (!isApiError(error, 401)) return
    if (!router.state.matches.some((match) => match.routeId === '/_app')) return
    queryClient.setQueryData(sessionQuery.queryKey, null)
    void router.navigate({ to: '/login', search: { redirect: router.state.location.href } })
  }

  return router
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof getRouter>
  }
}
