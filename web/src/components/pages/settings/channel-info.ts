import type { Schemas } from '~/api/client'
import { limits } from '~/api/limits'

export type ChannelEvent = Schemas['CreateNotificationRequest']['events'][number]
export type ChannelKind = Schemas['NotificationChannel']['kind']

const eventNames: Record<ChannelEvent, string> = {
  'job.failed': 'Job failed',
  'server.unreachable': 'Server unreachable',
  'disk.low': 'Disk almost full',
  'runner.deprecation': 'Runner version end date',
  'update.available': 'Update available',
  'backup.failed': 'Backup failed',
}

/** The events a channel can get, in the order of the API and of the dialog. */
export const channelEvents = limits.CreateNotificationRequest.events.items.enum.map((value) => ({ value, label: eventNames[value] }))

/** The events a new channel starts with. */
export const defaultEvents: ChannelEvent[] = ['job.failed', 'server.unreachable', 'disk.low']

/** The "Events" cell: the labels in one sentence, "Job failed, server unreachable, disk almost full". */
export function eventsSummary(events: string[]): string {
  const labels = events.map((event) => channelEvents.find((item) => item.value === event)?.label ?? event)
  return labels.map((label, index) => (index === 0 ? label : label[0]!.toLowerCase() + label.slice(1))).join(', ')
}

const kinds: Record<ChannelKind, { label: string; namePlaceholder: string; urlHelp: string }> = {
  slack: {
    label: 'Slack',
    namePlaceholder: '#ci-alerts',
    urlHelp: 'An incoming webhook URL from Slack. It is hidden after you save.',
  },
  discord: {
    label: 'Discord',
    namePlaceholder: '#builds',
    urlHelp: 'A channel webhook URL from Discord. It is hidden after you save.',
  },
  webhook: {
    label: 'Webhook',
    namePlaceholder: 'Ops webhook',
    urlHelp: 'runplane sends a JSON POST for each event. It is hidden after you save.',
  },
}

export const channelKinds = limits.CreateNotificationRequest.kind.enum.map((value) => ({ value, ...kinds[value] }))
