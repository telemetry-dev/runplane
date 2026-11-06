import { expect, test } from 'vitest'
import { api, apiUrl, toApiError } from './client'
import { formProblems } from './form-problems'

test('apiUrl encodes the path params and adds the query', () => {
  expect(apiUrl('/api/sandboxes/{id}/shell', { path: { id: 'a b/c' }, query: { rows: 24, cols: 80 } })).toBe(
    '/api/sandboxes/a%20b%2Fc/shell?rows=24&cols=80',
  )
  expect(apiUrl('/api/events')).toBe('/api/events')
})

test('an array query param goes as one comma-separated value, as the API reads it', async () => {
  let url = ''
  await api.GET('/api/pools/placement', {
    // Node has no page URL for a relative request.
    baseUrl: 'http://runplane.test',
    params: { query: { arch: 'x64', vcpu: 2, memory_mb: 4096, server_ids: ['s1', 's2'] } },
    fetch: async (request: Request) => {
      url = request.url
      return Response.json({ online_servers: 2, placeable_servers: 2, placeable_server_ids: ['s1', 's2'] })
    },
  })
  expect(new URL(url).search).toBe('?arch=x64&vcpu=2&memory_mb=4096&server_ids=s1,s2')
})

test('a validation problem gives its detail as message and its body fields as form errors', async () => {
  const response = new Response(
    JSON.stringify({
      title: 'Unprocessable Entity',
      status: 422,
      detail: 'validation failed',
      errors: [
        { message: 'expected length >= 8', location: 'body.new_password', value: 'short' },
        { message: 'expected number <= 200', location: 'query.limit', value: 500 },
      ],
    }),
    { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
  )

  const error = await toApiError(response)

  expect(error.status).toBe(422)
  expect(error.message).toBe('validation failed')
  expect(formProblems(error).fields).toEqual({ new_password: 'expected length >= 8' })
})

test('a non-JSON error page from a proxy falls back to the status text', async () => {
  const response = new Response('<html>502 Bad Gateway</html>', { status: 502, statusText: 'Bad Gateway' })

  const error = await toApiError(response)

  expect(error.status).toBe(502)
  expect(error.message).toBe('Bad Gateway')
  expect(formProblems(error).fields).toEqual({})
})
