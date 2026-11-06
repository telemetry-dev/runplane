import { queryOptions, useQuery, type QueryClient } from '@tanstack/react-query'
import { redirect } from '@tanstack/react-router'
import { $api, isApiError, type Schemas } from './client'

export const statusQuery = $api.queryOptions('get', '/api/status')

type Session = Schemas['Session']
type Capabilities = Schemas['Capabilities']

const session = $api.queryOptions('get', '/api/session')

/** The signed-in user with what the user can do, or null when there is no session. */
export const sessionQuery = queryOptions({
  queryKey: session.queryKey,
  queryFn: async (context): Promise<Session | null> => {
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
export async function requireSession(client: QueryClient, href: string): Promise<Session> {
  const user = await currentSession(client)
  if (!user) throw redirect({ to: '/login', search: { redirect: href } })
  return user
}

/** The session of the visitor, or null. Before the first admin exists, each guard sends the visitor to the setup. */
export async function currentSession(client: QueryClient): Promise<Session | null> {
  const status = await client.ensureQueryData(statusQuery)
  if (status.needs_setup) throw redirect({ to: '/setup' })
  return client.ensureQueryData(sessionQuery)
}

/**
 * What the role of the signed-in user allows, as the API reports it; nothing
 * while the session loads. `subscribed` is the useQuery option, which the
 * queries of the app frame set (see useFrameSubscribed).
 */
export function useCapabilities({ subscribed = true } = {}): Capabilities {
  return useQuery({ ...sessionQuery, subscribed }).data?.capabilities ?? nothing
}

/** useCapabilities for a loader, from the cached session. */
export function cachedCapabilities(client: QueryClient): Capabilities {
  return client.getQueryData(sessionQuery.queryKey)?.capabilities ?? nothing
}

const nothing: Capabilities = { admin: false, rerun_jobs: false, stop_sandboxes: false, open_shells: false }
