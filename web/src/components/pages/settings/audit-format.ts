import type { Schemas } from '~/api/client'
import { formatDateTime, formatRelativeTime, formatVersion } from '~/lib/format'
import { roleNames, roleWithArticle, type Role } from './member-info'

// The rows of Settings · Members › Activity, from the entries of GET /api/audit.

export type AuditEntry = Schemas['AuditEntry']
type Action = AuditEntry['action']

/** Who made the change: the user's name, or "runplane" for runplane itself. */
export function auditActor(entry: AuditEntry): string {
  if (!entry.user_id) return 'runplane'
  return entry.user_name || entry.user_email || 'A removed user'
}

/**
 * What the actor did, as the rest of the sentence after the name: "added the
 * label runplane-8cpu". An action of an older runplane shows as it is.
 */
export function auditChange(entry: AuditEntry): string {
  return changes[entry.action]?.(new Detail(entry.detail)) ?? `did ${entry.action}`
}

/** The changed object by name: a runner label, or another name such as "gra-b2-30". Undefined if the entry names none. */
export function auditTarget(entry: AuditEntry): { name: string; label: boolean } | undefined {
  const detail = new Detail(entry.detail)
  const label = detail.text('label')
  if (label) return { name: label, label: true }
  const name = detail.text('name') || detail.text('email') || detail.text('repository')
  return name ? { name, label: false } : undefined
}

/** "25 min ago" or "3 hours ago", then "Nov 3, 18:12" from the time the long relative form would say "1 day ago". */
export function auditTime(at: string, now = Date.now()): string {
  return Math.round((now - Date.parse(at)) / hour) < 24 ? formatRelativeTime(at, now, { long: true }) : formatDateTime(at)
}

const hour = 60 * 60 * 1000

class Detail {
  constructor(private readonly values: Record<string, unknown>) {}

  text(key: string): string {
    const value = this.values[key]
    return typeof value === 'string' ? value : ''
  }

  number(key: string): number | undefined {
    const value = this.values[key]
    return typeof value === 'number' ? value : undefined
  }

  is(key: string): boolean {
    return this.values[key] === true
  }

  list(key: string): string[] {
    const value = this.values[key]
    return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : []
  }

  /** The entry has the old and the new value of the key in its changes. */
  changed(key: string): boolean {
    const changes = this.values.changes
    return typeof changes === 'object' && changes !== null && Object.hasOwn(changes, key)
  }
}

// A new action of the API does not compile until it has a text here.
const changes: Record<Action, (detail: Detail) => string> = {
  'setup.done': () => 'set up runplane',
  'user.update': (d) => `made ${d.text('email') || 'a user'} ${roleText(d.text('role'))}`,
  'user.delete': (d) => `removed ${d.text('email') || 'a member'}`,
  'user.password': () => 'changed their password',
  'user.password_reset': (d) => `reset the password of ${d.text('email') || 'a user'}`,
  'invite.create': (d) => `invited ${d.text('email')} as ${roleText(d.text('role'))}`,
  'invite.link': (d) => `made a new invite link for ${d.text('email')}`,
  'invite.delete': () => 'revoked an invite',
  'pool.create': (d) => `added the label ${d.text('label')}`,
  'pool.update': (d) => `changed the label ${d.text('label')}`,
  'pool.delete': (d) => `removed the label ${d.text('label')}`,
  'server.token': (d) => `made an install command for ${d.text('name')}`,
  'server.install': (d) => `installed the server ${d.text('name')} over SSH`,
  'server.update': (d) => `${(d.changed('status') && nameIn(serverVerbs, d.text('status'))) || 'changed'} the server ${d.text('name')}`,
  'server.delete': (d) => `removed the server ${d.text('name')}`,
  'server.update_agent': (d) => `started the update of ${d.text('name')} to runplane ${formatVersion(d.text('to'))}`,
  'sandbox.stop': () => 'stopped a runner',
  'sandbox.shell': () => 'opened a debug shell',
  // The SSH gateway writes no user: the key of a GitHub user or an admin key opened the shell.
  'sandbox.ssh': (d) => `let ${d.text('github_login') ? `the GitHub user ${d.text('github_login')}` : 'an admin SSH key'} open a debug shell over SSH`,
  'cache.delete': (d) => {
    const count = d.number('deleted') ?? 0
    const entries = count === 1 ? '1 cache entry' : `${count} cache entries`
    const repository = d.text('repository')
    return repository ? `deleted ${entries} of ${repository}` : `deleted ${entries}`
  },
  'image.create': (d) => `added the image ${d.text('name')}`,
  'image.update': (d) => (d.is('is_default') ? `made ${d.text('name')} the default image` : `changed the image ${d.text('name')}`),
  'image.delete': (d) => `removed the image ${d.text('name')}`,
  'image.refresh': (d) => `refreshed the image ${d.text('name') || d.text('ref')}`,
  'settings.update': (d) => `changed ${settingNames(d.list('keys'))}`,
  'job.rerun': (d) => (d.is('debug') ? 're-ran a job with SSH' : 're-ran a job'),
  'backup.create': () => 'made a backup',
  'backup.download': () => 'downloaded a backup',
  'update.apply': () => 'started an update of runplane',
  'github.manifest': (d) => `started a GitHub App for ${d.text('organization') || 'a personal account'}`,
  'github.app.create': () => 'made the GitHub App',
  'github.installation.install': () => 'installed the GitHub App',
  'github.installation.update': () => 'changed the GitHub App installation',
  'github.installation.request': () => 'asked an owner to install the GitHub App',
  'github.sync': () => 'checked GitHub for new installations',
  'github.pull_request.create': (d) => `opened a pull request in ${d.text('repository')}`,
  'github.disconnect': () => 'disconnected GitHub',
  'notification.create': (d) => `added the ${nameIn(channelKinds, d.text('kind')) ?? 'notification'} channel ${d.text('name')}`,
  'notification.update': (d) => `changed the channel ${d.text('name')}`,
  'notification.delete': () => 'removed a notification channel',
}

const serverVerbs: Record<NonNullable<Schemas['UpdateServerRequest']['status']>, string> = {
  draining: 'drained',
  online: 'resumed',
  disabled: 'disabled',
}

const channelKinds: Record<Schemas['NotificationChannel']['kind'], string> = { slack: 'Slack', discord: 'Discord', webhook: 'webhook' }

/** The name of a value of an entry, or undefined for a value that the API no longer has. */
function nameIn<K extends string>(names: Record<K, string>, value: string): string | undefined {
  return Object.hasOwn(names, value) ? names[value as K] : undefined
}

/** "an admin", or the role as it is for a role that the API no longer has. */
function roleText(role: string): string {
  return Object.hasOwn(roleNames, role) ? roleWithArticle(role as Role) : role
}

/** "CPU overcommit", "the log retention and the debug hold", "3 settings". */
function settingNames(keys: string[]): string {
  const names = [...new Set(keys.map((key) => nameIn(settingNamesByKey, key) ?? key.replaceAll('_', ' ')))]
  if (names.length === 0) return 'the settings'
  if (names.length > 2) return `${names.length} settings`
  return names.join(' and ')
}

// The names of the settings pages, with "the" where a sentence needs it.
const settingNamesByKey: Record<keyof Schemas['SettingsPatch'], string> = {
  instance_name: 'the name',
  public_url: 'the dashboard URL',
  domain: 'the domain',
  acme_email: 'the contact email',
  mode: 'the sandbox mode',
  reserve_vcpu: 'the host reserve',
  reserve_memory_mb: 'the host reserve',
  cpu_overcommit: 'CPU overcommit',
  min_free_disk_percent: 'the minimum free disk',
  default_time_limit_minutes: 'the default time limit',
  allow_public_repos: 'public repositories',
  allow_fork_prs: 'fork pull requests',
  egress_allowlist: 'the egress allowlist',
  bandwidth_mbps: 'the bandwidth limit',
  dns_servers: 'the DNS servers',
  registry_mirror: 'the registry mirror',
  actions_cache: 'the Actions cache',
  cache_max_gb: 'the cache size limit',
  job_retention_days: 'the job retention',
  log_retention_days: 'the log retention',
  debug_hold_minutes: 'the debug hold',
  auto_update: 'automatic updates',
  backup_enabled: 'daily backups',
  backup_keep: 'the backups to keep',
  backup_s3: 'the S3 upload',
}
