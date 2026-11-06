import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { Ellipsis, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { useOptimisticPatch } from '~/api/optimistic-patch'
import { api, errorMessage, type Schemas } from '~/api/client'
import { cachedCapabilities, sessionQuery, useCapabilities } from '~/api/session'
import { membersQuery } from '~/api/users'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { DataTable, DataTableCell, DataTableHead, DataTableHeader, DataTableRow, useFittingColumns } from '~/components/app/data-table'
import { LoadError } from '~/components/app/load-error'
import { None } from '~/components/app/none'
import { SkeletonCell, SkeletonRows } from '~/components/app/skeleton-rows'
import { Avatar } from '~/components/ui/avatar'
import { IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuPopup, MenuTrigger } from '~/components/ui/menu'
import { Select, SelectItem, SelectPopup, SelectTrigger } from '~/components/ui/select'
import { toastManager } from '~/components/ui/toast'
import { formatRelativeTime } from '~/lib/format'
import { Activity, prefetchActivity } from './activity'
import { Invites } from './invites'
import { roles, roleWithArticle, type Members, type Role } from './member-info'
import { ReadOnlyNote, SettingsColumn, SettingsSection } from './parts'

type User = Schemas['User']

/** Starts the requests of the page, which are for admins only; the page shows its own loading and error states. */
export function prefetch(client: QueryClient) {
  if (!cachedCapabilities(client).admin) return
  void client.prefetchQuery(membersQuery)
  prefetchActivity(client)
}

/** Settings · Members: the users with their roles, the open invites, and the activity of all users. */
export function MemberSettings() {
  const { admin } = useCapabilities()
  const members = useQuery({ ...membersQuery, enabled: admin })
  const me = useQuery(sessionQuery).data
  // A narrow table drops Last login, then the avatar.
  const fit = useFittingColumns({ avatar: { width: 28, drop: 2 }, name: 'grow', role: 120, login: { width: 110, drop: 1 }, menu: 24 })

  if (!admin) {
    return (
      <SettingsColumn>
        <SettingsSection title="Members">
          <ReadOnlyNote>Only an admin can see and change members.</ReadOnlyNote>
        </SettingsSection>
      </SettingsColumn>
    )
  }

  const data = members.data
  return (
    <SettingsColumn>
      <SettingsSection title="Members" count={data?.users.length} table>
        {members.isError ? (
          <LoadError what="the members" error={members.error} onRetry={() => void members.refetch()} retrying={members.isFetching} />
        ) : (
          <div ref={fit.ref}>
            <DataTable aria-label="Members" columns={fit.columns}>
              <DataTableHeader>
                {fit.shown.avatar && <DataTableHead />}
                <DataTableHead>Name</DataTableHead>
                <DataTableHead>Role</DataTableHead>
                {fit.shown.login && <DataTableHead>Last login</DataTableHead>}
                <DataTableHead>
                  <span className="sr-only">Actions</span>
                </DataTableHead>
              </DataTableHeader>
              {data ? (
                data.users.map((user) => (
                  <UserRow
                    key={user.id}
                    user={user}
                    me={user.id === me?.id}
                    lastAdmin={user.id === data.last_admin}
                    capabilities={data.roles}
                    shown={fit.shown}
                  />
                ))
              ) : (
                <SkeletonRows label="Loading the members" count={2}>
                  {fit.shown.avatar && <SkeletonCell shape="large-avatar" />}
                  <SkeletonCell lines={[96, 120]} />
                  <SkeletonCell shape="select" />
                  {fit.shown.login && <SkeletonCell lines={[64]} />}
                  <SkeletonCell />
                </SkeletonRows>
              )}
            </DataTable>
          </div>
        )}
      </SettingsSection>
      {data && <Invites members={data} />}
      <Activity />
    </SettingsColumn>
  )
}

type Shown = Record<'avatar' | 'login', boolean>

type UserRowProps = {
  user: User
  me: boolean
  /** The only admin, which keeps its role. */
  lastAdmin: boolean
  /** What each role can do. */
  capabilities: Members['roles']
  shown: Shown
}

function UserRow({ user, me, lastAdmin, capabilities, shown }: UserRowProps) {
  const [dialog, setDialog] = useState<'demote' | 'remove'>()
  // Kept after the dialog closes, so its text stays while it fades out.
  const [demoteTo, setDemoteTo] = useState<Role>(user.role)
  const changeRole = useChangeRole()

  function pickRole(role: Role) {
    // Taking away your own admin role ends your access to settings, so ask first.
    if (me && !capabilities[role].admin) {
      setDemoteTo(role)
      setDialog('demote')
      return
    }
    changeRole.mutate(
      { user, role },
      { onError: (error) => toastManager.add({ title: `Could not change the role of ${user.name}`, description: errorMessage(error) }) },
    )
  }

  return (
    <DataTableRow>
      {shown.avatar && (
        <DataTableCell>
          <Avatar name={user.name} size="l" />
        </DataTableCell>
      )}
      <DataTableCell className="flex-col items-start gap-0">
        <span className="flex max-w-full items-baseline gap-xs">
          <span className="truncate font-medium text-neutral-10">{user.name}</span>
          {me && <span className="text-xs text-neutral-7">You</span>}
        </span>
        <span className="max-w-full truncate text-xs text-neutral-7">{user.email}</span>
      </DataTableCell>
      <DataTableCell>
        <Select items={roles} value={user.role} onValueChange={(role) => role && pickRole(role)} disabled={lastAdmin}>
          <SelectTrigger aria-label={`Role of ${user.name}`} className="w-[120px]" />
          <SelectPopup>
            {roles.map((role) => (
              <SelectItem key={role.value} value={role.value}>
                {role.label}
              </SelectItem>
            ))}
          </SelectPopup>
        </Select>
      </DataTableCell>
      {shown.login && (
        <DataTableCell>{user.last_login_at ? formatRelativeTime(user.last_login_at, Date.now(), { long: true }) : <None />}</DataTableCell>
      )}
      <DataTableCell align="end">
        {!me && (
          <>
            <Menu>
              <MenuTrigger render={<IconButton label={`${user.name} actions`} className="-m-xs" />}>
                <Ellipsis aria-hidden />
              </MenuTrigger>
              <MenuPopup align="end">
                <MenuItem icon={<Trash2 />} danger onClick={() => setDialog('remove')}>
                  Remove
                </MenuItem>
              </MenuPopup>
            </Menu>
            <RemoveUser user={user} open={dialog === 'remove'} onOpenChange={(open) => setDialog(open ? 'remove' : undefined)} />
          </>
        )}
        {me && (
          <ConfirmDialog
            open={dialog === 'demote'}
            onOpenChange={(open) => setDialog(open ? 'demote' : undefined)}
            title={`Make yourself ${roleWithArticle(demoteTo)}?`}
            description="You can no longer change settings or manage members. Another admin can make you an admin again."
            action={`Make me ${roleWithArticle(demoteTo)}`}
            onConfirm={() => changeRole.mutateAsync({ user, role: demoteTo })}
          />
        )}
      </DataTableCell>
    </DataTableRow>
  )
}

/** PATCH /api/users/{id} {role}. The select changes at once and changes back if the API refuses. */
function useChangeRole() {
  const client = useQueryClient()
  return useOptimisticPatch({
    queryKey: membersQuery.queryKey,
    mutationFn: ({ user, role }: { user: User; role: Role }) =>
      api.PATCH('/api/users/{id}', { params: { path: { id: user.id } }, body: { role } }),
    apply: (members: Members, { user, role }) => ({
      ...members,
      users: members.users.map((item) => (item.id === user.id ? { ...item, role } : item)),
    }),
    undo: ({ user }, before) => ({ user, role: before.users.find((item) => item.id === user.id)?.role ?? user.role }),
    onSuccess: () =>
      Promise.all([
        // A change of your own role changes what this page may show.
        client.invalidateQueries({ queryKey: sessionQuery.queryKey }),
        // The last admin can change.
        client.invalidateQueries({ queryKey: membersQuery.queryKey }),
      ]),
  })
}

type RemoveUserProps = { user: User; open: boolean; onOpenChange: (open: boolean) => void }

function RemoveUser({ user, open, onOpenChange }: RemoveUserProps) {
  const client = useQueryClient()
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Remove ${user.name}?`}
      description={`${user.name} is logged out and can no longer log in.`}
      action="Remove member"
      onConfirm={() => api.DELETE('/api/users/{id}', { params: { path: { id: user.id } } })}
      onDone={() => {
        client.setQueryData<Members>(membersQuery.queryKey, (old) => old && { ...old, users: old.users.filter((item) => item.id !== user.id) })
        // The last admin can change.
        void client.invalidateQueries({ queryKey: membersQuery.queryKey })
      }}
    />
  )
}
