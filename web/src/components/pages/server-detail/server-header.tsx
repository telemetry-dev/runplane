import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { CirclePause, CirclePlay, Ellipsis, Pencil, Stethoscope, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { $api, errorMessage, useFieldErrors } from '~/api/client'
import { BreadcrumbLink, BreadcrumbPage, Breadcrumbs } from '~/components/app/breadcrumbs'
import { StatusDot } from '~/components/app/status-dot'
import { dropServer } from '~/components/pages/servers/queries'
import { UpdateAgentItem } from '~/components/pages/servers/update-agent-item'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup } from '~/components/ui/alert-dialog'
import { Button, IconButton } from '~/components/ui/button'
import { Dialog, DialogClose, DialogFooter, DialogPopup } from '~/components/ui/dialog'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { Menu, MenuItem, MenuPopup, MenuTrigger } from '~/components/ui/menu'
import { toastManager } from '~/components/ui/toast'
import { putServer, serverQuery, type Doctor } from './queries'
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
  const paused = server.status === 'draining' || server.status === 'disabled'
  const update = $api.useMutation('patch', '/api/servers/{id}', {
    onSuccess: (updated) => {
      setConfirming(false)
      void putServer(client, updated)
    },
    onError: (error) => {
      if (paused) toastManager.add({ title: `Could not resume ${server.name}`, description: errorMessage(error) })
    },
  })

  function setStatus(status: 'online' | 'draining') {
    update.mutate({ params: { path: { id: server.id } }, body: { status } })
  }

  if (paused) {
    return (
      <Button loading={update.isPending} onClick={() => setStatus('online')}>
        {!update.isPending && <CirclePlay aria-hidden />}
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
      <AlertDialog open={confirming} onOpenChange={setConfirming} onOpenChangeComplete={(open) => !open && update.reset()}>
        <AlertDialogPopup title={`Drain ${server.name}?`} description="New jobs go to other servers. Running jobs finish.">
          {update.error && <ErrorText error={update.error} />}
          <AlertDialogFooter>
            <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
            <Button variant="primary" loading={update.isPending} onClick={() => setStatus('draining')}>
              Drain server
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>
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
      void putServer(client, updated)
    },
  })
  const errors = useFieldErrors(rename.error)

  return (
    <Dialog open={open} onOpenChange={onOpenChange} onOpenChangeComplete={(isOpen) => !isOpen && rename.reset()}>
      <DialogPopup title="Rename server">
        <Form
          errors={errors}
          onFormSubmit={(values) => rename.mutate({ params: { path: { id: server.id } }, body: { name: String(values.name).trim() } })}
          className="gap-l"
        >
          <Field name="name" label="Name" validate={(value) => (String(value).trim() ? null : 'Enter a name.')}>
            <Input defaultValue={server.name} maxLength={64} autoComplete="off" spellCheck={false} />
          </Field>
          {rename.error && Object.keys(errors).length === 0 && <ErrorText error={rename.error} />}
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
  const remove = $api.useMutation('delete', '/api/servers/{id}', {
    onSuccess: () => {
      toastManager.add({ title: `Removed ${server.name}` })
      void navigate({ to: '/servers' }).then(() => {
        client.removeQueries({ queryKey: serverQuery(server.id).queryKey })
        void dropServer(client, server.id)
      })
    },
  })

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange} onOpenChangeComplete={(isOpen) => !isOpen && remove.reset()}>
      <AlertDialogPopup title={`Remove ${server.name}?`} description={removeDescription(server.running_jobs)}>
        {remove.error && <ErrorText error={remove.error} />}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="danger" loading={remove.isPending} onClick={() => remove.mutate({ params: { path: { id: server.id } } })}>
            Remove server
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}

function removeDescription(runningJobs: number): string {
  if (runningJobs === 0) return 'runplane stops its sandboxes and disconnects the agent.'
  const jobs = runningJobs === 1 ? 'Its running job is' : `Its ${runningJobs} running jobs are`
  return `${jobs} cancelled, and runplane disconnects the agent.`
}

function ErrorText({ error }: { error: unknown }) {
  return (
    <p role="alert" className="text-s text-error">
      {errorMessage(error)}
    </p>
  )
}
