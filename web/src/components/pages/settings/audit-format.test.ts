import { describe, expect, test } from 'vitest'
import { auditActor, auditChange, auditTarget, auditTime, type AuditEntry } from './audit-format'

// The database can hold the actions of an older runplane, so action is any text.
function entry(action: string, detail: Record<string, unknown> = {}, user: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: '01m6audit',
    at: '2026-11-04T11:35:00Z',
    user_id: '01m3ada',
    user_name: 'Ada Okafor',
    user_email: 'ada@kestrel.dev',
    action: action as AuditEntry['action'],
    target: '01m3target',
    detail,
    ip: '203.0.113.24',
    ...user,
  }
}

describe('auditChange', () => {
  test.each([
    ['user.update', { email: 'mira@kestrel.dev', role: 'admin' }, 'made mira@kestrel.dev an admin'],
    ['user.update', { email: 'mira@kestrel.dev', role: 'member' }, 'made mira@kestrel.dev a member'],
    [
      'server.update',
      { name: 'fsn1-ax52-a', status: 'draining', changes: { status: ['online', 'draining'] } },
      'drained the server fsn1-ax52-a',
    ],
    [
      'server.update',
      { name: 'fsn1-ax52-a', status: 'online', changes: { status: ['draining', 'online'] } },
      'resumed the server fsn1-ax52-a',
    ],
    // A rename of an online server does not resume it.
    [
      'server.update',
      { name: 'fsn1-ax52-b', status: 'online', changes: { name: ['fsn1-ax52-a', 'fsn1-ax52-b'] } },
      'changed the server fsn1-ax52-b',
    ],
    ['server.update_agent', { name: 'fsn1-ax52-a', from: 'v0.4.1', to: 'v0.4.2' }, 'started the update of fsn1-ax52-a to runplane v0.4.2'],
    ['notification.create', { name: 'ops', kind: 'slack' }, 'added the Slack channel ops'],
    ['notification.create', { name: 'ops', kind: 'pager' }, 'added the notification channel ops'],
    ['cache.delete', { repository: 'kestrel-hq/mobile', id: '', deleted: 214 }, 'deleted 214 cache entries of kestrel-hq/mobile'],
    ['cache.delete', { repository: '', id: 'c1', deleted: 1 }, 'deleted 1 cache entry'],
    ['image.update', { name: 'ubuntu-24.04', is_default: true }, 'made ubuntu-24.04 the default image'],
    ['image.update', { name: 'ubuntu-24.04', is_default: null }, 'changed the image ubuntu-24.04'],
    ['job.rerun', { debug: true }, 're-ran a job with SSH'],
    ['job.rerun', { debug: false }, 're-ran a job'],
    ['sandbox.ssh', { key: 'SHA256:x', github_login: 'octocat' }, 'let the GitHub user octocat open a debug shell over SSH'],
    ['sandbox.ssh', { key: 'SHA256:x', admin_key: true }, 'let an admin SSH key open a debug shell over SSH'],
    ['settings.update', { keys: ['cpu_overcommit'] }, 'changed CPU overcommit'],
    // Host reserve is two keys, but one setting.
    ['settings.update', { keys: ['log_retention_days', 'reserve_memory_mb', 'reserve_vcpu'] }, 'changed the log retention and the host reserve'],
    ['settings.update', { keys: ['domain', 'acme_email', 'public_url'] }, 'changed 3 settings'],
    ['settings.update', { keys: ['new_setting'] }, 'changed new setting'],
  ])('%s %j', (action, detail, want) => {
    expect(auditChange(entry(action, detail))).toBe(want)
  })

  test('an action of an older runplane shows as it is', () => {
    expect(auditChange(entry('server.reboot'))).toBe('did server.reboot')
  })
})

test('auditActor names the user, the email of a user without a name, runplane, or a removed user', () => {
  expect(auditActor(entry('pool.create'))).toBe('Ada Okafor')
  expect(auditActor(entry('pool.create', {}, { user_name: '' }))).toBe('ada@kestrel.dev')
  expect(auditActor(entry('backup.create', {}, { user_id: '', user_name: '', user_email: '' }))).toBe('runplane')
  expect(auditActor(entry('pool.create', {}, { user_name: '', user_email: '' }))).toBe('A removed user')
})

test('auditTarget names the object from the detail, never by its ID', () => {
  expect(auditTarget(entry('pool.delete', { label: 'runplane-4cpu' }))).toEqual({ name: 'runplane-4cpu', label: true })
  expect(auditTarget(entry('server.delete', { name: 'gra-b2-30' }))).toEqual({ name: 'gra-b2-30', label: false })
  expect(auditTarget(entry('cache.delete', { repository: 'kestrel-hq/web', id: '' }))).toEqual({ name: 'kestrel-hq/web', label: false })
  expect(auditTarget(entry('backup.create'))).toBeUndefined()
})

test('auditTime counts minutes and hours, then shows the date instead of "1 day ago"', () => {
  const now = new Date(2026, 10, 4, 12, 0).getTime()
  expect(auditTime(new Date(2026, 10, 4, 11, 35).toISOString(), now)).toBe('25 min ago')
  expect(auditTime(new Date(2026, 10, 3, 13, 0).toISOString(), now)).toBe('23 hours ago')
  expect(auditTime(new Date(2026, 10, 3, 12, 30).toISOString(), now)).toBe('Nov 3, 12:30')
})
