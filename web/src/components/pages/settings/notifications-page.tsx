import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Ellipsis, Pencil, Plus, Send, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { $api, api, errorMessage, type Schemas } from '~/api/client'
import { DataTable, DataTableCell, DataTableHead, DataTableHeader, DataTableRow, useFittingColumns } from '~/components/app/data-table'
import { CheckIcon } from '~/components/app/status-icon'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup } from '~/components/ui/alert-dialog'
import { Button, IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuTrigger } from '~/components/ui/menu'
import { Switch } from '~/components/ui/switch'
import { toastManager } from '~/components/ui/toast'
import { ChannelDialog } from './channel-dialog'
import { ChannelTile } from './channel-icons'
import { eventsSummary } from './channel-info'
import { notificationsQuery, useSendTest } from './notifications-api'
import { relativeTime } from './format'
import { Bar, ErrorHint, LoadError, ReadOnlyNote, SettingsColumn, SettingsSection } from './parts'
import { useIsAdmin } from './settings-api'

type Channel = Schemas['NotificationChannel']

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
          <LoadError what="notification channels" onRetry={() => void channels.refetch()} retrying={channels.isFetching} />
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
                <SkeletonRows shown={fit.shown} />
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
  const when = channel.last_sent_at ? ` ${relativeTime(channel.last_sent_at)}` : ''
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
  const client = useQueryClient()
  const setEnabled = (id: string, enabled: boolean) =>
    client.setQueryData<Channel[]>(notificationsQuery.queryKey, (list) =>
      list?.map((item) => (item.id === id ? { ...item, enabled } : item)),
    )

  return useMutation({
    mutationFn: ({ channel, enabled }: { channel: Channel; enabled: boolean }) =>
      api.PATCH('/api/notifications/{id}', { params: { path: { id: channel.id } }, body: { enabled } }),
    onMutate: async ({ channel, enabled }) => {
      await client.cancelQueries({ queryKey: notificationsQuery.queryKey })
      setEnabled(channel.id, enabled)
    },
    onError: (error, { channel }) => {
      setEnabled(channel.id, channel.enabled)
      toastManager.add({ title: `Could not change ${channel.name}`, description: errorMessage(error) })
    },
  })
}

type DeleteChannelProps = { channel: Channel; open: boolean; onOpenChange: (open: boolean) => void }

function DeleteChannel({ channel, open, onOpenChange }: DeleteChannelProps) {
  const client = useQueryClient()
  const remove = $api.useMutation('delete', '/api/notifications/{id}', { onSuccess: () => onOpenChange(false) })

  return (
    <AlertDialog
      open={open}
      onOpenChange={onOpenChange}
      onOpenChangeComplete={(isOpen) => {
        if (isOpen) return
        // Drop the row only after the close animation; the dialog lives in it.
        if (remove.isSuccess)
          client.setQueryData<Channel[]>(notificationsQuery.queryKey, (list) => list?.filter((item) => item.id !== channel.id))
        remove.reset()
      }}
    >
      <AlertDialogPopup title={`Delete ${channel.name}?`} description="runplane stops sending events to this channel.">
        {remove.error && (
          <p role="alert" className="text-s text-error">
            {errorMessage(remove.error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="danger" loading={remove.isPending} onClick={() => remove.mutate({ params: { path: { id: channel.id } } })}>
            Delete channel
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
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

function SkeletonRows({ shown }: { shown: Shown }) {
  return Array.from({ length: 2 }, (_, index) => (
    <DataTableRow key={index}>
      {shown.icon && (
        <DataTableCell>
          <Bar className="size-[28px] rounded-s" />
        </DataTableCell>
      )}
      <DataTableCell className="flex-col items-start gap-0">
        <Bar className="my-[3px] h-[12px] w-[96px] rounded-xs" />
        <Bar className="my-[2px] h-[10px] w-[180px] max-w-full rounded-xs" />
      </DataTableCell>
      {shown.events && (
        <DataTableCell>
          <Bar className="my-[3px] h-[12px] w-[200px] rounded-xs" />
        </DataTableCell>
      )}
      <DataTableCell>
        <Bar className="h-[18px] w-[32px] rounded-full" />
      </DataTableCell>
      <DataTableCell />
    </DataTableRow>
  ))
}
