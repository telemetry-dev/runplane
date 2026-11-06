import { afterEach, expect, test, vi } from 'vitest'
import { findOrganizations } from './organizations'

// The fields that api.github.com sends for an organization.
function org(login: string, id: number) {
  return {
    login,
    id,
    node_id: `O_${id}`,
    url: `https://api.github.com/orgs/${login}`,
    avatar_url: `https://avatars.githubusercontent.com/u/${id}?v=4`,
    description: null,
  }
}

const limited = { message: 'API rate limit exceeded for 203.0.113.7.', documentation_url: 'https://docs.github.com/rest' }

/** Answers the two GitHub requests: the exact login and the search. */
function github(exact: [number, object], search: [number, object]) {
  const fetch = vi.fn(async (url: string) => {
    const [status, body] = new URL(url).pathname.startsWith('/orgs/') ? exact : search
    return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

afterEach(() => {
  vi.unstubAllGlobals()
})

test('the organization with the exact login comes first, one time', async () => {
  const fetch = github(
    [200, { ...org('telemetry-dev', 278555771), name: null, type: 'Organization', public_repos: 3 }],
    [200, { total_count: 2, incomplete_results: false, items: [{ ...org('telemetry-digital', 9), type: 'Organization', score: 1 }, { ...org('Telemetry-Dev', 278555771), type: 'Organization', score: 1 }] }],
  )
  await expect(findOrganizations('telemetry-dev')).resolves.toEqual([
    { login: 'telemetry-dev', avatar_url: 'https://avatars.githubusercontent.com/u/278555771?v=4' },
    { login: 'telemetry-digital', avatar_url: 'https://avatars.githubusercontent.com/u/9?v=4' },
  ])
  // Without type:org, the search also finds the accounts of users.
  const search = fetch.mock.calls.map(([url]) => new URL(url)).find((url) => url.pathname === '/search/users')
  expect(search?.searchParams.get('q')).toBe('telemetry-dev type:org')
})

test('a search limit does not hide the organization with the exact login', async () => {
  github([200, { ...org('acme', 10), name: 'Acme', type: 'Organization' }], [403, limited])
  await expect(findOrganizations('acme')).resolves.toEqual([{ login: 'acme', avatar_url: 'https://avatars.githubusercontent.com/u/10?v=4' }])
})

test('a search limit without an exact login is an error, not an empty list', async () => {
  github([404, { message: 'Not Found', documentation_url: 'https://docs.github.com/rest' }], [403, limited])
  await expect(findOrganizations('acm')).rejects.toThrow('GitHub permits 10 searches a minute')
})

// Other than searches, GitHub permits 60 requests an hour from one IP address.
// A text that cannot be a login does not use one of them.
test('a name with a space is not a login, so only the search runs', async () => {
  const fetch = github(
    [404, { message: 'Not Found', documentation_url: 'https://docs.github.com/rest' }],
    [200, { total_count: 1, incomplete_results: false, items: [{ ...org('acme-corp', 11), type: 'Organization', score: 1 }] }],
  )
  await expect(findOrganizations('Acme Corp')).resolves.toEqual([{ login: 'acme-corp', avatar_url: 'https://avatars.githubusercontent.com/u/11?v=4' }])
  expect(fetch.mock.calls.map(([url]) => new URL(url).pathname)).toEqual(['/search/users'])
})
