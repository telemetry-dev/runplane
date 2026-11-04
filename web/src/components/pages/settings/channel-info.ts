import type { Schemas } from '~/api/client'

export type ChannelEvent = Schemas['CreateNotificationRequest']['events'][number]
export type ChannelKind = Schemas['NotificationChannel']['kind']

/** The events a channel can get, in the order of the dialog. */
export const channelEvents: { value: ChannelEvent; label: string }[] = [
  { value: 'job.failed', label: 'Job failed' },
  { value: 'server.unreachable', label: 'Server unreachable' },
  { value: 'disk.low', label: 'Disk almost full' },
  { value: 'runner.deprecation', label: 'Runner version end date' },
  { value: 'update.available', label: 'Update available' },
  { value: 'backup.failed', label: 'Backup failed' },
]

/** The events a new channel starts with. */
export const defaultEvents: ChannelEvent[] = ['job.failed', 'server.unreachable', 'disk.low']

/** The "Events" cell: the labels in one sentence, "Job failed, server unreachable, disk almost full". */
export function eventsSummary(events: string[]): string {
  const labels = events.map((event) => channelEvents.find((item) => item.value === event)?.label ?? event)
  return labels.map((label, index) => (index === 0 ? label : label[0]!.toLowerCase() + label.slice(1))).join(', ')
}

export const channelKinds: { value: ChannelKind; label: string; namePlaceholder: string; urlHelp: string }[] = [
  {
    value: 'slack',
    label: 'Slack',
    namePlaceholder: '#ci-alerts',
    urlHelp: 'An incoming webhook URL from Slack. It is hidden after you save.',
  },
  {
    value: 'discord',
    label: 'Discord',
    namePlaceholder: '#builds',
    urlHelp: 'A channel webhook URL from Discord. It is hidden after you save.',
  },
  {
    value: 'webhook',
    label: 'Webhook',
    namePlaceholder: 'Ops webhook',
    urlHelp: 'runplane sends a JSON POST for each event. It is hidden after you save.',
  },
]
