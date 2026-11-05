import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { Ellipsis, Pencil, Plus, Send, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { api, errorMessage, type Schemas } from '~/api/client'
import { notificationsQuery } from '~/api/notifications'
import { useOptimisticPatch } from '~/api/optimistic-patch'
import { isAdmin, useIsAdmin } from '~/api/session'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { DataTable, DataTableCell, DataTableHead, DataTableHeader, DataTableRow, useFittingColumns } from '~/components/app/data-table'
import { ErrorHint } from '~/components/app/error-hint'
import { LoadError } from '~/components/app/load-error'
import { SkeletonCell, SkeletonRows } from '~/components/app/skeleton-rows'
import { CheckIcon } from '~/components/app/status-icon'
import { Button, IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuTrigger } from '~/components/ui/menu'
import { Switch } from '~/components/ui/switch'
import { toastManager } from '~/components/ui/toast'
import { formatRelativeTime } from '~/lib/format'
import { ChannelDialog } from './channel-dialog'
import { ChannelTile } from './channel-icons'
import { eventsSummary } from './channel-info'
import { ReadOnlyNote, SettingsColumn, SettingsSection } from './parts'
import { useSendTest } from './send-test'

type Channel = Schemas['NotificationChannel']

/** Starts the request of the page, which is for admins only; the page shows its own loading and error states. */
export function prefetch(client: QueryClient) {
  if (isAdmin(client)) void client.prefetchQuery(notificationsQuery)
}

/** Settings · Notifications: the Slack, Discord, and webhook channels that get runplane events. */
export function NotificationSettings() {
  const admin = useIsAdmin()
  const channels = useQuery({ ...notificationsQuery, enabled: admin })
  const [adding, setAdding] = useState(false)
  const hasChannels = (channels.data?.length ?? 0) > 0
  // A narrow table drops Events, then the channel icon.
  const fit = useFittingColumns({ icon: { width: 28, drop: 2 }, channel: 'grow', events: { width: 260, drop: 1 }, on: 32, menu: 24 })

  return (
    <SettingsColumn>
      <SettingsSection
        title="Channels"
        table
        action={
          hasChannels && (
            <Button onClick={() => setAdding(true)}>
              <Plus aria-hidden />
              Add channel
            </Button>
          )
        }
      >
        {!admin ? (
          <ReadOnlyNote>Only an admin can see and change notification channels.</ReadOnlyNote>
        ) : channels.isError ? (
          <LoadError what="the notification channels" error={channels.error} onRetry={() => void channels.refetch()} retrying={channels.isFetching} />
        ) : channels.data?.length === 0 ? (
          <NoChannels onAdd={() => setAdding(true)} />
        ) : (
          <div ref={fit.ref}>
            <DataTable aria-label="Channels" columns={fit.columns}>
              <DataTableHeader>
                {fit.shown.icon && <DataTableHead />}
                <DataTableHead>Channel</DataTableHead>
                {fit.shown.events && <DataTableHead>Events</DataTableHead>}
                <DataTableHead>On</DataTableHead>
                <DataTableHead>
                  <span className="sr-only">Actions</span>
                </DataTableHead>
              </DataTableHeader>
              {channels.data ? (
                channels.data.map((channel) => <ChannelRow key={channel.id} channel={channel} shown={fit.shown} />)
              ) : (
                <SkeletonRows label="Loading the channels" count={2}>
                  {fit.shown.icon && <SkeletonCell shape="tile" />}
                  <SkeletonCell lines={[96, 180]} />
                  {fit.shown.events && <SkeletonCell lines={[200]} />}
                  <SkeletonCell shape="switch" />
                  <SkeletonCell />
                </SkeletonRows>
              )}
            </DataTable>
          </div>
        )}
      </SettingsSection>
      <ChannelDialog open={adding} onOpenChange={setAdding} />
    </SettingsColumn>
  )
}

type Shown = Record<'icon' | 'events', boolean>

function ChannelRow({ channel, shown }: { channel: Channel; shown: Shown }) {
  const [dialog, setDialog] = useState<'edit' | 'delete'>()
  const enable = useEnableChannel()
  const sendTest = useSendTest()

  return (
    <DataTableRow>
      {shown.icon && (
        <DataTableCell>
          <ChannelTile kind={channel.kind} />
        </DataTableCell>
      )}
      <DataTableCell className="flex-col items-start gap-0">
        <span className="max-w-full truncate font-medium text-neutral-10">{channel.name}</span>
        <span className="max-w-full truncate font-code text-xs text-neutral-7">{channel.url}</span>
        <LastDelivery channel={channel} />
      </DataTableCell>
      {shown.events && (
        <DataTableCell>
          {channel.events.length > 0 ? (
            <span className="text-pretty">{eventsSummary(channel.events)}</span>
          ) : (
            <span className="text-neutral-7">No events</span>
          )}
        </DataTableCell>
      )}
      <DataTableCell>
        <Switch
          aria-label={`Send events to ${channel.name}`}
          checked={channel.enabled}
          onCheckedChange={(enabled) => enable.mutate({ channel, enabled })}
        />
      </DataTableCell>
      <DataTableCell align="end">
        <Menu>
          <MenuTrigger render={<IconButton label={`${channel.name} actions`} className="-m-xs" />}>
            <Ellipsis aria-hidden />
          </MenuTrigger>
          <MenuPopup align="end">
            <MenuItem icon={<Pencil />} onClick={() => setDialog('edit')}>
              Edit
            </MenuItem>
            <MenuItem icon={<Send />} onClick={() => sendTest.mutate({ name: channel.name, id: channel.id })}>
              Send test
            </MenuItem>
            <MenuSeparator />
            <MenuItem icon={<Trash2 />} danger onClick={() => setDialog('delete')}>
              Delete
            </MenuItem>
          </MenuPopup>
        </Menu>
        <ChannelDialog channel={channel} open={dialog === 'edit'} onOpenChange={(open) => setDialog(open ? 'edit' : undefined)} />
        <DeleteChannel channel={channel} open={dialog === 'delete'} onOpenChange={(open) => setDialog(open ? 'delete' : undefined)} />
      </DataTableCell>
    </DataTableRow>
  )
}

/** A failed last delivery, with the error in a tooltip. A channel that works shows nothing more. */
function LastDelivery({ channel }: { channel: Channel }) {
  if (!channel.last_error) return null
  const when = channel.last_sent_at ? ` ${formatRelativeTime(channel.last_sent_at, Date.now(), { long: true })}` : ''
  return (
    <ErrorHint error={channel.last_error}>
      <span className="mt-xxs flex items-center gap-xs text-xs font-medium text-neutral-10">
        <CheckIcon status="warn" size={14} />
        Last message failed{when}
      </span>
    </ErrorHint>
  )
}

/** PATCH {enabled}. The switch moves at once and moves back if the API refuses. */
function useEnableChannel() {
  return useOptimisticPatch({
    queryKey: notificationsQuery.queryKey,
    mutationFn: ({ channel, enabled }: { channel: Channel; enabled: boolean }) =>
      api.PATCH('/api/notifications/{id}', { params: { path: { id: channel.id } }, body: { enabled } }),
    apply: (channels: Channel[], { channel, enabled }) => channels.map((item) => (item.id === channel.id ? { ...item, enabled } : item)),
    undo: ({ channel }, before) => ({ channel, enabled: before.find((item) => item.id === channel.id)?.enabled ?? channel.enabled }),
    onError: (error, { channel }) => toastManager.add({ title: `Could not change ${channel.name}`, description: errorMessage(error) }),
  })
}

type DeleteChannelProps = { channel: Channel; open: boolean; onOpenChange: (open: boolean) => void }

function DeleteChannel({ channel, open, onOpenChange }: DeleteChannelProps) {
  const client = useQueryClient()
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Delete ${channel.name}?`}
      description="runplane stops sending events to this channel."
      action="Delete channel"
      onConfirm={() => api.DELETE('/api/notifications/{id}', { params: { path: { id: channel.id } } })}
      onDone={() => client.setQueryData<Channel[]>(notificationsQuery.queryKey, (list) => list?.filter((item) => item.id !== channel.id))}
    />
  )
}

/** The compact empty state of the design: a bordered panel with one action. */
function NoChannels({ onAdd }: { onAdd: () => void }) {
  return (
    <div className="relative flex flex-col items-start gap-s rounded-m p-xl after:pointer-events-none after:absolute after:inset-0 after:rounded-[inherit] after:shadow-[inset_0_0_0_1px_var(--neutral-4)]">
      <h3 className="text-m font-heavy tracking-l text-neutral-10">No channels yet</h3>
      <p className="max-w-[440px] text-s text-pretty text-neutral-8">
        Send failed jobs, unreachable servers, and other events to Slack, Discord, or your own webhook.
      </p>
      <div className="pt-s">
        <Button onClick={onAdd}>
          <Plus aria-hidden />
          Add channel
        </Button>
      </div>
    </div>
  )
}
