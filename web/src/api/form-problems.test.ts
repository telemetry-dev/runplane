import { expect, test } from 'vitest'
import { ApiError } from './client'
import { formProblems } from './form-problems'

const fields = ['name', 'password']

test('without an error there are no problems', () => {
  expect(formProblems(null, { fields })).toEqual({ fields: {}, alert: undefined })
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
  expect(formProblems(error, { fields })).toEqual({
    fields: { password: 'The password must have at least 10 characters.' },
    alert: undefined,
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
  expect(formProblems(error, { fields })).toEqual({ fields: {}, alert: 'A user with this email exists.' })
})

test('a problem without fields goes to the alert', () => {
  const error = new ApiError(401, { type: 'about:blank', title: 'Unauthorized', status: 401, detail: 'The email or password is wrong.' }, 'Unauthorized')
  expect(formProblems(error, { fields: ['email', 'password'] })).toEqual({ fields: {}, alert: 'The email or password is wrong.' })
})

function invalid(...errors: { location: string; message: string }[]) {
  return new ApiError(422, { type: 'about:blank', title: 'Unprocessable Entity', status: 422, detail: 'validation failed', errors }, 'Unprocessable Entity')
}

test('a problem with an item of a list goes under the field of the list', () => {
  const error = invalid({ location: 'body.server_ids[1]', message: 'No server has this ID.' }, { location: 'body.image_id', message: 'No image has this ID.' })
  expect(formProblems(error, { fields: ['server_ids', 'image_id'] })).toEqual({
    fields: { server_ids: 'No server has this ID.', image_id: 'No image has this ID.' },
    alert: undefined,
  })
})

test('the alert names the problems that no field shows, also next to field problems', () => {
  const error = invalid({ location: 'body.arch', message: 'Use x64 or arm64.' }, { location: 'body.name', message: 'Enter a name.' })
  expect(formProblems(error, { fields })).toEqual({ fields: { name: 'Enter a name.' }, alert: 'Use x64 or arm64.' })
})

test('the prefix comes off the field names', () => {
  const error = invalid({ location: 'body.backup_s3.bucket', message: 'Enter a bucket.' })
  expect(formProblems(error, { prefix: 'backup_s3.' })).toEqual({ fields: { bucket: 'Enter a bucket.' }, alert: undefined })
})
