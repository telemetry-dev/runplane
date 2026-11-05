import { queryOptions, useQuery, type QueryClient } from '@tanstack/react-query'
import { redirect } from '@tanstack/react-router'
import { $api, isApiError, type Schemas } from './client'

export const statusQuery = $api.queryOptions('get', '/api/status')

const session = $api.queryOptions('get', '/api/session')

/** The signed-in user, or null when there is no session. */
export const sessionQuery = queryOptions({
  queryKey: session.queryKey,
  queryFn: async (context): Promise<Schemas['User'] | null> => {
    try {
      return await session.queryFn(context)
    } catch (error) {
      if (isApiError(error, 401)) return null
      throw error
    }
  },
})

/**
 * The guard of the pages for signed-in users. Without a session the visitor
 * goes to the login page, and back to `href` after it.
 */
export async function requireSession(client: QueryClient, href: string): Promise<Schemas['User']> {
  const user = await currentSession(client)
  if (!user) throw redirect({ to: '/login', search: { redirect: href } })
  return user
}

/** The session of the visitor, or null. Before the first admin exists, each guard sends the visitor to the setup. */
export async function currentSession(client: QueryClient): Promise<Schemas['User'] | null> {
  const status = await client.ensureQueryData(statusQuery)
  if (status.needs_setup) throw redirect({ to: '/setup' })
  return client.ensureQueryData(sessionQuery)
}

/** `subscribed` is the useQuery option, which the queries of the app frame set (see useFrameSubscribed). */
export function useIsAdmin({ subscribed = true } = {}): boolean {
  return useQuery({ ...sessionQuery, subscribed }).data?.role === 'admin'
}

/** useIsAdmin for a loader, from the cached session. */
export function isAdmin(client: QueryClient): boolean {
  return client.getQueryData(sessionQuery.queryKey)?.role === 'admin'
}
