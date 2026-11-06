import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Copy, RotateCw } from 'lucide-react'
import { useState } from 'react'
import { $api, api, errorMessage, type Schemas } from '~/api/client'
import { useFormProblems } from '~/api/form-problems'
import { membersQuery } from '~/api/users'
import { copyText } from '~/components/app/copy-button'
import { DataTable, DataTableCell, DataTableHead, DataTableHeader, DataTableRow, useFittingColumns } from '~/components/app/data-table'
import { Button } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { Select, SelectItem, SelectPopup, SelectTrigger } from '~/components/ui/select'
import { toastManager } from '~/components/ui/toast'
import { formatDate, formatRelativeTime } from '~/lib/format'
import { roles, roleSummary, type Members, type Role } from './member-info'
import { quietAction, SettingsCard, SettingsSection } from './parts'

type Invite = Schemas['Invite']

/**
 * The Invites section: a form that makes an invite link and copies it, and the
 * open invites. The API returns a link only when it makes the invite, so only
 * invites made on this page can copy their link again; the others get a new one.
 */
export function Invites({ members }: { members: Schemas['Members'] }) {
  const [links, setLinks] = useState<Record<string, string>>({})
  const remember = (invite: Invite, url: string) => setLinks((old) => ({ ...old, [invite.id]: url }))
  const names = new Map(members.users.map((user) => [user.id, user.name]))
  const { invites } = members
  // A narrow table drops Expires, then Role.
  const fit = useFittingColumns({ email: 'grow', role: { width: 80, drop: 2 }, expires: { width: 100, drop: 1 }, actions: 150 })

  return (
    <SettingsSection title="Invites" count={invites.length || undefined} table>
      <div className="flex flex-col gap-l">
        <InviteForm onCreated={remember} members={roleSummary('member', members.roles.member)} />
        {invites.length > 0 && (
          <div ref={fit.ref}>
            <DataTable aria-label="Invites" columns={fit.columns}>
              <DataTableHeader>
                <DataTableHead>Email</DataTableHead>
                {fit.shown.role && <DataTableHead>Role</DataTableHead>}
                {fit.shown.expires && <DataTableHead>Expires</DataTableHead>}
                <DataTableHead>
                  <span className="sr-only">Actions</span>
                </DataTableHead>
              </DataTableHeader>
              {invites.map((invite) => (
                <InviteRow
                  key={invite.id}
                  invite={invite}
                  invitedBy={names.get(invite.created_by)}
                  link={links[invite.id]}
                  onNewLink={remember}
                  shown={fit.shown}
                />
              ))}
            </DataTable>
          </div>
        )}
      </div>
    </SettingsSection>
  )
}

type InviteFormProps = {
  onCreated: (invite: Invite, url: string) => void
  /** What members can do. */
  members: string
}

function InviteForm({ onCreated, members }: InviteFormProps) {
  const client = useQueryClient()
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('member')
  // A new key gives an empty form that has not been submitted, so the cleared email shows no "required" error.
  const [formKey, setFormKey] = useState(0)
  const create = $api.useMutation('post', '/api/invites', {
    onSuccess: ({ invite, url }) => {
      // A new invite replaces the open invite of the same email.
      client.setQueryData<Members>(
        membersQuery.queryKey,
        (old) => old && { ...old, invites: [invite, ...old.invites.filter((item) => item.email !== invite.email)] },
      )
      onCreated(invite, url)
      setEmail('')
      setFormKey((key) => key + 1)
      return copyLink(url, 'Invite link copied')
    },
  })
  const problems = useFormProblems(create.error)

  return (
    <SettingsCard className="gap-m">
      <Form key={formKey} errors={problems.fields} onFormSubmit={() => create.mutate({ body: { email: email.trim(), role } })}>
        <div className="flex flex-col gap-s @min-[480px]:flex-row @min-[480px]:items-start">
          <Field name="email" label="Email" className="min-w-0 flex-1">
            <Input type="email" value={email} onValueChange={setEmail} placeholder="name@example.com" required autoComplete="off" />
          </Field>
          <Field name="role" label="Role" className="@min-[480px]:w-[140px] @min-[480px]:shrink-0">
            <Select name="role" items={roles} value={role} onValueChange={(value) => value && setRole(value)}>
              <SelectTrigger />
              <SelectPopup>
                {roles.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          </Field>
          {/* Level with the inputs: below the label line and the field gap. */}
          <Button type="submit" loading={create.isPending} className="self-start @min-[480px]:mt-[calc(var(--line-s)+var(--space-xs))]">
            Create invite
          </Button>
        </div>
        {problems.alert && (
          <p role="alert" className="text-s text-error">
            {problems.alert}
          </p>
        )}
        <p className="text-xs text-pretty text-neutral-7">
          Each link works once, until it expires. {members}
        </p>
      </Form>
    </SettingsCard>
  )
}

type InviteRowProps = {
  invite: Invite
  invitedBy: string | undefined
  /** The link, if this page made the invite. */
  link: string | undefined
  onNewLink: (invite: Invite, url: string) => void
  shown: Record<'role' | 'expires', boolean>
}

function InviteRow({ invite, invitedBy, link, onNewLink, shown }: InviteRowProps) {
  const client = useQueryClient()
  const setInvites = (change: (invites: Invite[]) => Invite[]) =>
    client.setQueryData<Members>(membersQuery.queryKey, (old) => old && { ...old, invites: change(old.invites) })

  const revoke = useMutation({
    mutationFn: () => api.DELETE('/api/invites/{id}', { params: { path: { id: invite.id } } }),
    onMutate: () => setInvites((invites) => invites.filter((item) => item.id !== invite.id)),
    onError: (error) => {
      void client.invalidateQueries({ queryKey: membersQuery.queryKey })
      toastManager.add({ title: `Could not revoke the invite for ${invite.email}`, description: errorMessage(error) })
    },
  })

  // The invite gets a new ID and link; the old link stops working.
  const renew = $api.useMutation('post', '/api/invites/{id}/link', {
    onSuccess: ({ invite: renewed, url }) => {
      setInvites((invites) => invites.map((item) => (item.id === invite.id ? renewed : item)))
      onNewLink(renewed, url)
      return copyLink(url, 'New invite link copied')
    },
    onError: (error) => toastManager.add({ title: `Could not make a new link for ${invite.email}`, description: errorMessage(error) }),
  })

  return (
    <DataTableRow>
      <DataTableCell className="flex-col items-start gap-0">
        <span className="max-w-full truncate font-medium text-neutral-10">{invite.email}</span>
        <span className="max-w-full truncate text-xs text-neutral-7">
          {invitedBy ? `Invited by ${invitedBy} · ` : 'Invited '}
          {formatDate(invite.created_at, { year: false })}
        </span>
      </DataTableCell>
      {shown.role && <DataTableCell>{roles.find((role) => role.value === invite.role)?.label}</DataTableCell>}
      {shown.expires && <DataTableCell>{formatRelativeTime(invite.expires_at, Date.now(), { long: true })}</DataTableCell>}
      <DataTableCell align="end">
        <div className="flex items-center gap-l">
          {link ? (
            <button type="button" className={quietAction} onClick={() => void copyLink(link, 'Invite link copied')}>
              <Copy aria-hidden />
              Copy link
            </button>
          ) : (
            <button
              type="button"
              className={quietAction}
              disabled={renew.isPending}
              onClick={() => renew.mutate({ params: { path: { id: invite.id } } })}
            >
              <RotateCw aria-hidden />
              New link
            </button>
          )}
          <button
            type="button"
            className={quietAction}
            onClick={() => revoke.mutate()}
            aria-label={`Revoke the invite for ${invite.email}`}
          >
            Revoke
          </button>
        </div>
      </DataTableCell>
    </DataTableRow>
  )
}

/** Copies an invite link. Without clipboard access the toast shows the link to copy by hand. */
async function copyLink(url: string, title: string) {
  try {
    await copyText(url)
    toastManager.add({ title })
  } catch {
    toastManager.add({ title: 'Copy this invite link', description: <LinkField url={url} />, timeout: 0 })
  }
}

/**
 * The link in a field that selects it on focus. A toast's own text cannot be
 * selected, since a drag on it swipes the toast away; a drag in a field selects.
 */
function LinkField({ url }: { url: string }) {
  return (
    <Input
      readOnly
      value={url}
      aria-label="Invite link"
      onFocus={(event) => event.currentTarget.select()}
      className="mt-xxs font-code text-xs select-text read-only:text-neutral-10 dark:read-only:bg-neutral-3"
    />
  )
}
