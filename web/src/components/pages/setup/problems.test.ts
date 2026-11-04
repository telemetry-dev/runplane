import { expect, test } from 'vitest'
import { ApiError } from '~/api/client'
import { submitProblems } from './problems'

const fields = ['name', 'password']

test('without an error there are no problems', () => {
  expect(submitProblems(null, fields)).toEqual({ errors: {}, alert: null })
})

test('a field problem goes under its field, and there is no alert', () => {
  const error = new ApiError(
    422,
    {
      type: 'about:blank',
      title: 'Unprocessable Entity',
      status: 422,
      detail: 'The password must have at least 10 characters.',
      errors: [{ location: 'body.password', message: 'The password must have at least 10 characters.' }],
    },
    'Unprocessable Entity',
  )
  expect(submitProblems(error, fields)).toEqual({
    errors: { password: 'The password must have at least 10 characters.' },
    alert: null,
  })
})

test('a problem about a field the form does not show goes to the alert', () => {
  // Accepting an invite: the form has no email field.
  const error = new ApiError(
    409,
    {
      type: 'about:blank',
      title: 'Conflict',
      status: 409,
      detail: 'A user with this email exists.',
      errors: [{ location: 'body.email', message: 'A user with this email exists.' }],
    },
    'Conflict',
  )
  expect(submitProblems(error, fields)).toEqual({ errors: {}, alert: 'A user with this email exists.' })
})

test('a problem without fields goes to the alert', () => {
  const error = new ApiError(401, { type: 'about:blank', title: 'Unauthorized', status: 401, detail: 'The email or password is wrong.' }, 'Unauthorized')
  expect(submitProblems(error, ['email', 'password'])).toEqual({ errors: {}, alert: 'The email or password is wrong.' })
})
