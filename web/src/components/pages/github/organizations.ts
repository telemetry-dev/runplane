import { limits } from '~/api/limits'

// The organization search of the Connect GitHub step. The browser asks the
// public GitHub API, which needs no token; GitHub then permits 10 searches a
// minute and 60 other requests an hour for each IP address.

export type Organization = { login: string; avatar_url: string }

const organization = limits.ManifestRequest.organization
const loginPattern = new RegExp(organization.pattern)

/** Whether `text` can be the login of an organization, as the API accepts it. */
function isLogin(text: string): boolean {
  return text.length <= organization.maxLength && loginPattern.test(text)
}

/**
 * Organizations for the search text. The organization whose login is the text
 * comes first, so its full name always finds it, also after GitHub limits the
 * search.
 */
export async function findOrganizations(text: string, signal?: AbortSignal): Promise<Organization[]> {
  const search = new URLSearchParams({ per_page: '8', q: `${text} type:org` })
  const [exact, found] = await Promise.allSettled([
    isLogin(text) ? github<Organization>(`/orgs/${encodeURIComponent(text)}`, signal) : Promise.reject(new Error('not a login')),
    github<{ items: Organization[] }>(`/search/users?${search}`, signal),
  ])
  const orgs = [...(exact.status === 'fulfilled' ? [exact.value] : []), ...(found.status === 'fulfilled' ? found.value.items : [])]
  if (orgs.length === 0 && found.status === 'rejected') throw found.reason
  const seen = new Set<string>()
  return orgs
    .filter((org) => !seen.has(org.login.toLowerCase()) && seen.add(org.login.toLowerCase()))
    .map((org) => ({ login: org.login, avatar_url: org.avatar_url }))
}

async function github<T>(path: string, signal?: AbortSignal): Promise<T> {
  let response: Response
  try {
    response = await fetch(`https://api.github.com${path}`, { signal, headers: { Accept: 'application/vnd.github+json' } })
  } catch (error) {
    if (signal?.aborted) throw error
    throw new Error('This browser cannot connect to api.github.com.')
  }
  if (response.status === 403 || response.status === 429) {
    throw new Error('GitHub permits 10 searches a minute. Type the full name of the organization, or wait a minute.')
  }
  if (!response.ok) throw new Error(`GitHub answered with status ${response.status}.`)
  return (await response.json()) as T
}
