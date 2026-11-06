import { QueryClient } from '@tanstack/react-query'
import { expect, test } from 'vitest'
import type { Schemas } from './client'
import { currentSession, requireSession, sessionQuery, statusQuery } from './session'

const status: Schemas['Status'] = {
  has_jobs: true,
  instance_name: 'Kestrel CI',
  mode: 'vm',
  needs_setup: false,
  systems: [],
  version: 'v0.4.2 (3f2a9c1)',
}

const ada: Schemas['Session'] = {
  id: '01m3ada',
  name: 'Ada Okafor',
  email: 'ada@kestrel.dev',
  role: 'admin',
  created_at: '2026-10-02T04:49:45Z',
  last_login_at: '2026-11-04T04:49:45Z',
  capabilities: { admin: true, rerun_jobs: true, stop_sandboxes: true, open_shells: true },
}

/** A cache with the status and the session, as the guards read them. */
function cached(status: Schemas['Status'], session: Schemas['Session'] | null) {
  const client = new QueryClient()
  client.setQueryData(statusQuery.queryKey, status)
  client.setQueryData(sessionQuery.queryKey, session)
  return client
}

test('before the first admin exists, the guards send the visitor to the setup', async () => {
  const client = cached({ ...status, needs_setup: true }, null)
  await expect(requireSession(client, '/jobs')).rejects.toMatchObject({ options: { to: '/setup' } })
  await expect(currentSession(client)).rejects.toMatchObject({ options: { to: '/setup' } })
})

test('without a session, requireSession sends the visitor to the login page, which opens the page again after it', async () => {
  await expect(requireSession(cached(status, null), '/jobs?status=failed')).rejects.toMatchObject({
    options: { to: '/login', search: { redirect: '/jobs?status=failed' } },
  })
})

test('the guards give the session of a signed-in user, and currentSession gives null without one', async () => {
  await expect(requireSession(cached(status, ada), '/jobs')).resolves.toEqual(ada)
  await expect(currentSession(cached(status, ada))).resolves.toEqual(ada)
  await expect(currentSession(cached(status, null))).resolves.toBeNull()
})
