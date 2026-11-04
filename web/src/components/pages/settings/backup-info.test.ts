import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { backupStatus, storedOn } from './backup-info'

function backup(fields: Partial<Schemas['Backup']>): Schemas['Backup'] {
  return {
    id: '01m6backup',
    status: 'done',
    available: true,
    s3_key: '',
    size_bytes: 19_503_513,
    error: '',
    created_at: '2026-11-04T03:00:00Z',
    ...fields,
  }
}

test('stored on names every place that has the file', () => {
  expect(storedOn(backup({}))).toBe('This server')
  expect(storedOn(backup({ status: 'uploaded', s3_key: 'ci/2026-11-04.db' }))).toBe('This server · S3')
  expect(storedOn(backup({ status: 'uploaded', s3_key: 'ci/2026-11-04.db', available: false }))).toBe('S3')
  expect(storedOn(backup({ status: 'uploading' }))).toBe('This server · uploading to S3')
})

test('a failed upload keeps the local file; a failed backup has none', () => {
  expect(storedOn(backup({ status: 'failed', error: 'AccessDenied' }))).toBe('This server · S3 upload failed')
  expect(storedOn(backup({ status: 'failed', available: false, error: 'disk full' }))).toBe('')
  expect(storedOn(backup({ status: 'running', available: false }))).toBe('')
})

test('an upload shows as running', () => {
  expect(backupStatus(backup({ status: 'uploading' }))).toBe('running')
  expect(backupStatus(backup({ status: 'uploaded' }))).toBe('succeeded')
})
