import { expect, test } from 'vitest'
import { fieldErrors, toApiError } from './client'

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
  expect(fieldErrors(error)).toEqual({ new_password: 'expected length >= 8' })
})

test('a non-JSON error page from a proxy falls back to the status text', async () => {
  const response = new Response('<html>502 Bad Gateway</html>', { status: 502, statusText: 'Bad Gateway' })

  const error = await toApiError(response)

  expect(error.status).toBe(502)
  expect(error.message).toBe('Bad Gateway')
  expect(fieldErrors(error)).toEqual({})
})
