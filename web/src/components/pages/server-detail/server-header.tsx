import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { CirclePause, CirclePlay, Ellipsis, Pencil, Stethoscope, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { $api, api, errorMessage } from '~/api/client'
import { applyChange, dropServer } from '~/api/events'
import { useFormProblems } from '~/api/form-problems'
import { limits } from '~/api/limits'
import type { Doctor } from '~/api/servers'
import { BreadcrumbLink, BreadcrumbPage, Breadcrumbs } from '~/components/app/breadcrumbs'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { StatusDot } from '~/components/app/status-dot'
import { removeDescription } from '~/components/pages/servers/server-info'
import { UpdateAgentItem } from '~/components/pages/servers/update-agent-item'
import { Button, IconButton } from '~/components/ui/button'
import { Dialog, DialogClose, DialogFooter, DialogPopup } from '~/components/ui/dialog'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { Menu, MenuItem, MenuPopup, MenuTrigger } from '~/components/ui/menu'
import { toastManager } from '~/components/ui/toast'
import { canRunDoctor, type Server } from './server-format'

type ServerHeaderProps = {
  server: Server
  /** Shows the actions; they need the admin role. */
  admin: boolean
  doctor: Doctor
}

/** Breadcrumbs, the name with the status dot, and Run doctor, Drain or Resume, and the More menu. */
export function ServerHeader({ server, admin, doctor }: ServerHeaderProps) {
  return (
    <header className="flex flex-col gap-s">
      <Breadcrumbs>
        <BreadcrumbLink to="/servers">Servers</BreadcrumbLink>
        <BreadcrumbPage>{server.name}</BreadcrumbPage>
      </Breadcrumbs>
      <div className="flex flex-wrap items-center justify-between gap-m">
        <div className="flex min-w-0 items-center gap-s">
          <StatusDot status={server.status} size={22} />
          <h1 className="type-page-title truncate text-neutral-10">{server.name}</h1>
        </div>
        {admin && (
          <div className="flex flex-wrap items-center gap-xs">
            <Button loading={doctor.running} disabled={!canRunDoctor(server)} onClick={doctor.run}>
              {!doctor.running && <Stethoscope aria-hidden />}
              Run doctor
            </Button>
            {/* An installing server cannot change its status yet (the API answers 409). */}
            {server.status !== 'installing' && <DrainButton server={server} />}
            <MoreMenu server={server} />
          </div>
        )}
      </div>
    </header>
  )
}

function DrainButton({ server }: { server: Server }) {
  const client = useQueryClient()
  const [confirming, setConfirming] = useState(false)
  const resume = $api.useMutation('patch', '/api/servers/{id}', {
    onSuccess: (updated) => applyChange(client, 'server.state', updated),
    onError: (error) => toastManager.add({ title: `Could not resume ${server.name}`, description: errorMessage(error) }),
  })

  if (server.status === 'draining' || server.status === 'disabled') {
    return (
      <Button loading={resume.isPending} onClick={() => resume.mutate({ params: { path: { id: server.id } }, body: { status: 'online' } })}>
        {!resume.isPending && <CirclePlay aria-hidden />}
        Resume
      </Button>
    )
  }
  return (
    <>
      <Button onClick={() => setConfirming(true)}>
        <CirclePause aria-hidden />
        Drain
      </Button>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={`Drain ${server.name}?`}
        description="New jobs go to other servers. Running jobs finish."
        action="Drain server"
        variant="primary"
        onConfirm={async () => {
          const { data } = await api.PATCH('/api/servers/{id}', { params: { path: { id: server.id } }, body: { status: 'draining' } })
          void applyChange(client, 'server.state', data!)
        }}
      />
    </>
  )
}

function MoreMenu({ server }: { server: Server }) {
  const [dialog, setDialog] = useState<'rename' | 'remove'>()
  return (
    <>
      <Menu>
        <MenuTrigger
          render={<IconButton label="More" variant="secondary" />}
        >
          <Ellipsis aria-hidden />
        </MenuTrigger>
        <MenuPopup align="end">
          <MenuItem icon={<Pencil />} onClick={() => setDialog('rename')}>
            Rename
          </MenuItem>
          <UpdateAgentItem server={server} />
          {/* runplane runs on the primary, so it cannot be removed. */}
          {server.kind !== 'local' && (
            <MenuItem icon={<Trash2 />} danger onClick={() => setDialog('remove')}>
              Remove server
            </MenuItem>
          )}
        </MenuPopup>
      </Menu>
      <RenameDialog server={server} open={dialog === 'rename'} onOpenChange={(open) => setDialog(open ? 'rename' : undefined)} />
      <RemoveDialog server={server} open={dialog === 'remove'} onOpenChange={(open) => setDialog(open ? 'remove' : undefined)} />
    </>
  )
}

type DialogProps = { server: Server; open: boolean; onOpenChange: (open: boolean) => void }

function RenameDialog({ server, open, onOpenChange }: DialogProps) {
  const client = useQueryClient()
  const rename = $api.useMutation('patch', '/api/servers/{id}', {
    onSuccess: (updated) => {
      onOpenChange(false)
      void applyChange(client, 'server.state', updated)
    },
  })
  const problems = useFormProblems(rename.error)

  return (
    <Dialog open={open} onOpenChange={onOpenChange} onOpenChangeComplete={(isOpen) => !isOpen && rename.reset()}>
      <DialogPopup title="Rename server">
        <Form
          errors={problems.fields}
          onFormSubmit={(values) => rename.mutate({ params: { path: { id: server.id } }, body: { name: String(values.name).trim() } })}
          className="gap-l"
        >
          <Field name="name" label="Name" validate={(value) => (String(value).trim() ? null : 'Enter a name.')}>
            <Input defaultValue={server.name} maxLength={limits.UpdateServerRequest.name.maxLength} autoComplete="off" spellCheck={false} />
          </Field>
          {problems.alert && <ErrorText error={problems.alert} />}
          <DialogFooter>
            <DialogClose render={<Button />}>Cancel</DialogClose>
            <Button type="submit" variant="primary" loading={rename.isPending}>
              Save
            </Button>
          </DialogFooter>
        </Form>
      </DialogPopup>
    </Dialog>
  )
}

function RemoveDialog({ server, open, onOpenChange }: DialogProps) {
  const client = useQueryClient()
  const navigate = useNavigate()
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Remove ${server.name}?`}
      description={removeDescription(server.running_jobs)}
      action="Remove server"
      onConfirm={async () => {
        await api.DELETE('/api/servers/{id}', { params: { path: { id: server.id } } })
        toastManager.add({ title: `Removed ${server.name}` })
        void navigate({ to: '/servers' }).then(() => dropServer(client, server.id))
      }}
    />
  )
}

function ErrorText({ error }: { error: unknown }) {
  return (
    <p role="alert" className="text-s text-error">
      {errorMessage(error)}
    </p>
  )
}
