import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Ellipsis, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { $api, api, errorMessage, type Schemas } from '~/api/client'
import { sessionQuery, useIsAdmin } from '~/api/session'
import { membersQuery } from '~/api/users'
import { DataTable, DataTableCell, DataTableHead, DataTableHeader, DataTableRow, useFittingColumns } from '~/components/app/data-table'
import { None } from '~/components/app/none'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup } from '~/components/ui/alert-dialog'
import { Avatar } from '~/components/ui/avatar'
import { Button, IconButton } from '~/components/ui/button'
import { Menu, MenuItem, MenuPopup, MenuTrigger } from '~/components/ui/menu'
import { Select, SelectItem, SelectPopup, SelectTrigger } from '~/components/ui/select'
import { Skeleton } from '~/components/ui/skeleton'
import { toastManager } from '~/components/ui/toast'
import { formatRelativeTime } from '~/lib/format'
import { Activity } from './activity'
import { Invites } from './invites'
import { roles, type Members, type Role } from './member-info'
import { LoadError, ReadOnlyNote, SettingsColumn, SettingsSection } from './parts'

type User = Schemas['User']

/** Settings · Members: the users with their roles, the open invites, and the activity of all users. */
export function MemberSettings() {
  const admin = useIsAdmin()
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

  const users = members.data?.users
  const admins = users?.filter((user) => user.role === 'admin').length ?? 0
  return (
    <SettingsColumn>
      <SettingsSection title="Members" count={users?.length} table>
        {members.isError ? (
          <LoadError what="members" onRetry={() => void members.refetch()} retrying={members.isFetching} />
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
              {users ? (
                users.map((user) => (
                  <UserRow
                    key={user.id}
                    user={user}
                    me={user.id === me?.id}
                    onlyAdmin={user.role === 'admin' && admins === 1}
                    shown={fit.shown}
                  />
                ))
              ) : (
                <SkeletonRows shown={fit.shown} />
              )}
            </DataTable>
          </div>
        )}
      </SettingsSection>
      {members.data && <Invites members={members.data} />}
      <Activity />
    </SettingsColumn>
  )
}

type Shown = Record<'avatar' | 'login', boolean>

type UserRowProps = { user: User; me: boolean; onlyAdmin: boolean; shown: Shown }

function UserRow({ user, me, onlyAdmin, shown }: UserRowProps) {
  const [dialog, setDialog] = useState<'demote' | 'remove'>()
  const changeRole = useChangeRole()

  function pickRole(role: Role) {
    // Taking away your own admin role ends your access to settings, so ask first.
    if (me && role === 'member') setDialog('demote')
    else changeRole.mutate({ user, role })
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
        <Select items={roles} value={user.role} onValueChange={(role) => role && pickRole(role)} disabled={onlyAdmin}>
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
          <DemoteMyself
            open={dialog === 'demote'}
            onOpenChange={(open) => setDialog(open ? 'demote' : undefined)}
            onConfirm={() => changeRole.mutate({ user, role: 'member' }, { onSettled: () => setDialog(undefined) })}
            pending={changeRole.isPending}
          />
        )}
      </DataTableCell>
    </DataTableRow>
  )
}

/** PATCH /api/users/{id} {role}. The select changes at once and changes back if the API refuses. */
function useChangeRole() {
  const client = useQueryClient()
  const setRole = (id: string, role: Role) =>
    client.setQueryData<Members>(
      membersQuery.queryKey,
      (old) => old && { ...old, users: old.users.map((item) => (item.id === id ? { ...item, role } : item)) },
    )

  return useMutation({
    mutationFn: ({ user, role }: { user: User; role: Role }) =>
      api.PATCH('/api/users/{id}', { params: { path: { id: user.id } }, body: { role } }),
    onMutate: async ({ user, role }) => {
      await client.cancelQueries({ queryKey: membersQuery.queryKey })
      setRole(user.id, role)
    },
    onError: (error, { user }) => {
      setRole(user.id, user.role)
      toastManager.add({ title: `Could not change the role of ${user.name}`, description: errorMessage(error) })
    },
    // A change of your own role changes what this page may show.
    onSuccess: () => client.invalidateQueries({ queryKey: sessionQuery.queryKey }),
  })
}

type DemoteMyselfProps = { open: boolean; onOpenChange: (open: boolean) => void; onConfirm: () => void; pending: boolean }

function DemoteMyself({ open, onOpenChange, onConfirm, pending }: DemoteMyselfProps) {
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogPopup
        title="Make yourself a member?"
        description="You can no longer change settings or manage members. Another admin can make you an admin again."
      >
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="danger" loading={pending} onClick={onConfirm}>
            Make me a member
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}

type RemoveUserProps = { user: User; open: boolean; onOpenChange: (open: boolean) => void }

function RemoveUser({ user, open, onOpenChange }: RemoveUserProps) {
  const client = useQueryClient()
  const remove = $api.useMutation('delete', '/api/users/{id}', { onSuccess: () => onOpenChange(false) })

  return (
    <AlertDialog
      open={open}
      onOpenChange={onOpenChange}
      onOpenChangeComplete={(isOpen) => {
        if (isOpen) return
        // Drop the row only after the close animation; the dialog lives in it.
        if (remove.isSuccess) {
          client.setQueryData<Members>(
            membersQuery.queryKey,
            (old) => old && { ...old, users: old.users.filter((item) => item.id !== user.id) },
          )
        }
        remove.reset()
      }}
    >
      <AlertDialogPopup title={`Remove ${user.name}?`} description={`${user.name} is logged out and can no longer log in.`}>
        {remove.error && (
          <p role="alert" className="text-s text-error">
            {errorMessage(remove.error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="danger" loading={remove.isPending} onClick={() => remove.mutate({ params: { path: { id: user.id } } })}>
            Remove member
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}

function SkeletonRows({ shown }: { shown: Shown }) {
  return Array.from({ length: 2 }, (_, index) => (
    <DataTableRow key={index}>
      {shown.avatar && (
        <DataTableCell>
          <Skeleton className="size-[28px] rounded-full" />
        </DataTableCell>
      )}
      <DataTableCell className="flex-col items-start gap-0">
        <Skeleton className="my-[3px] h-[12px] w-[96px]" />
        <Skeleton className="my-[2px] h-[10px] w-[120px]" />
      </DataTableCell>
      <DataTableCell>
        <Skeleton className="h-[calc(var(--line-s)+2*var(--space-xs))] w-[120px] rounded-s" />
      </DataTableCell>
      {shown.login && (
        <DataTableCell>
          <Skeleton className="my-[3px] h-[12px] w-[64px]" />
        </DataTableCell>
      )}
      <DataTableCell />
    </DataTableRow>
  ))
}
