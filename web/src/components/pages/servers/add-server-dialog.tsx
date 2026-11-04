import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useEffect, useId, useRef, useState, type ReactElement } from 'react'
import { $api, errorMessage, fieldErrors, useFieldErrors, type Schemas } from '~/api/client'
import { CodeBlock } from '~/components/app/code-block'
import { Button } from '~/components/ui/button'
import { Dialog, DialogClose, DialogDescription, DialogFooter, DialogPopup, DialogTrigger } from '~/components/ui/dialog'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { Spinner } from '~/components/ui/spinner'
import { Tab, Tabs, TabsList, TabsPanel } from '~/components/ui/tabs'
import { InstallProgress } from './install-progress'
import { refreshServers, serversQuery } from './queries'
import { commandLines, installState } from './server-info'

type AddServerDialogProps = {
  /** The element that opens the dialog. The default is the primary "Add server" button of the Servers page. */
  trigger?: ReactElement
}

/** The "Add server" dialog: an install command, or an install over SSH. */
export function AddServerDialog({ trigger = defaultTrigger }: AddServerDialogProps) {
  const [open, setOpen] = useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={trigger} />
      <DialogPopup title="Add server" width={520}>
        <AddServer onDone={() => setOpen(false)} />
      </DialogPopup>
    </Dialog>
  )
}

const defaultTrigger = (
  <Button variant="primary">
    <Plus aria-hidden />
    Add server
  </Button>
)

type Method = 'script' | 'ssh'

// The dialog content unmounts when the dialog closes, so each opening starts
// with a new install token.
function AddServer({ onDone }: { onDone: () => void }) {
  const [method, setMethod] = useState<Method>('script')
  const script = useScriptInstall()
  const ssh = useSshInstall()

  // Each tab locks the other once its install starts, so the tab names the install.
  const server = method === 'script' ? script.server : ssh.server
  const connected = installState(server) === 'connected'
  const started = script.used || ssh.server !== undefined

  // An SSH install disables the focused form, and an expired token needs a new
  // command; the footer button takes the focus for the next step.
  const primary = useRef<HTMLButtonElement>(null)
  useEffect(() => {
    if (ssh.server || script.expired) primary.current?.focus()
  }, [ssh.server, script.expired])

  const footer =
    method === 'script' && !started
      ? script.expired || script.error
        ? { label: 'New command', onClick: script.request }
        : { label: 'Done', disabled: true }
      : method === 'ssh' && !started
        ? { label: 'Connect', type: 'submit' as const, form: ssh.formId }
        : { label: 'Done', onClick: onDone, disabled: !connected }

  return (
    <>
      <Tabs value={method} onValueChange={setMethod}>
        <TabsList aria-label="Connection">
          <Tab value="script" disabled={ssh.server !== undefined}>
            Install script
          </Tab>
          <Tab value="ssh" disabled={script.used}>
            Connect over SSH
          </Tab>
        </TabsList>
        <TabsPanel value="script" className="mt-l">
          <DialogDescription>
            Run this on a fresh Ubuntu 22.04+ or Debian 12 server. It installs Docker and the runplane agent, then connects to{' '}
            <PublicHost />.
          </DialogDescription>
          <InstallCommand script={script} />
        </TabsPanel>
        <TabsPanel value="ssh" className="mt-l">
          <SshForm ssh={ssh} />
        </TabsPanel>
      </Tabs>

      {(method === 'script' ? script.token && !script.error : ssh.server) && (
        <InstallProgress server={server} expired={method === 'script' && script.expired} />
      )}

      <DialogFooter>
        {!connected && <DialogClose render={<Button />}>Cancel</DialogClose>}
        {/* One button through all steps, so it keeps the focus when the form submits. */}
        <Button
          ref={primary}
          variant="primary"
          type={footer.type ?? 'button'}
          form={footer.form}
          onClick={footer.onClick}
          disabled={footer.disabled}
          focusableWhenDisabled
          loading={script.pending || ssh.pending}
        >
          {footer.label}
        </Button>
      </DialogFooter>
    </>
  )
}

/**
 * An install token, requested when the dialog opens, and the server that used
 * it. Its status says when an agent has used the token.
 */
function useScriptInstall() {
  const client = useQueryClient()
  const createToken = $api.useMutation('post', '/api/servers/tokens')
  const { mutate } = createToken
  const token = createToken.data

  // A ref, not state, so that StrictMode's second effect run makes no second token.
  const requested = useRef(false)
  useEffect(() => {
    if (requested.current) return
    requested.current = true
    mutate({})
  }, [mutate])

  const status = $api.useQuery(
    'get',
    '/api/servers/tokens/{id}/status',
    { params: { path: { id: token?.token_id ?? '' } } },
    { enabled: token !== undefined, refetchInterval: (query) => (query.state.data?.used ? false : 3000) },
  ).data
  const serverId = status?.used ? status.server_id : undefined
  const servers = useQuery(serversQuery).data
  const server = serverId ? servers?.find((item) => item.id === serverId) : undefined

  // The list may not have the new server yet.
  useEffect(() => {
    if (serverId) void refreshServers(client)
  }, [client, serverId])

  return {
    token,
    error: createToken.error,
    pending: createToken.isPending,
    request: () => mutate({}),
    used: status?.used === true,
    expired: useExpired(token && !status?.used ? token.expires_at : undefined),
    server,
  }
}

type ScriptInstall = ReturnType<typeof useScriptInstall>

function InstallCommand({ script }: { script: ScriptInstall }) {
  if (script.error) {
    return (
      <p role="alert" className="text-s text-error">
        {errorMessage(script.error)}
      </p>
    )
  }
  if (!script.token) {
    return (
      <div className="flex items-center gap-s rounded-s bg-(--code-background) p-m text-s text-neutral-7">
        <Spinner size={14} />
        Making an install token
      </div>
    )
  }
  return (
    <>
      <CodeBlock code={commandLines(script.token.command)} copyable wrap />
      <p className="text-xs text-neutral-7">The token works once and expires in 1 hour.</p>
    </>
  )
}

function useSshInstall() {
  const client = useQueryClient()
  const formId = useId()
  const installSsh = $api.useMutation('post', '/api/servers/ssh', { onSuccess: () => void refreshServers(client) })
  const started = installSsh.data
  const servers = useQuery(serversQuery).data
  return {
    formId,
    connect: (body: Schemas['SSHInstall']) => installSsh.mutate({ body }),
    error: installSsh.error,
    pending: installSsh.isPending,
    server: started && (servers?.find((server) => server.id === started.id) ?? started),
  }
}

function SshForm({ ssh }: { ssh: ReturnType<typeof useSshInstall> }) {
  const key = $api.useQuery('get', '/api/servers/ssh-key', undefined, { staleTime: Infinity })
  const errors = useFieldErrors(ssh.error)
  const disabled = ssh.server !== undefined

  return (
    <>
      <DialogDescription>
        Add this key to <code className="font-code text-xs">~/.ssh/authorized_keys</code> on a fresh Ubuntu 22.04+ or Debian 12 server.
        runplane then installs Docker and the agent over SSH.
      </DialogDescription>
      {key.data ? (
        <CodeBlock code={key.data.public_key} copyable wrap />
      ) : key.error ? (
        <div role="alert" className="flex items-center justify-between gap-m text-s text-neutral-8">
          <p className="min-w-0">Could not load the SSH key. {errorMessage(key.error)}</p>
          <Button onClick={() => void key.refetch()} loading={key.isFetching}>
            Try again
          </Button>
        </div>
      ) : (
        <div className="flex items-center gap-s rounded-s bg-(--code-background) p-m text-s text-neutral-7">
          <Spinner size={14} />
          Loading the SSH key
        </div>
      )}
      <Form
        id={ssh.formId}
        errors={errors}
        onFormSubmit={(values) =>
          ssh.connect({
            host: String(values.host).trim(),
            port: Number(values.port),
            user: String(values.user).trim(),
            name: String(values.name).trim(),
          })
        }
        className="mt-xs"
      >
        <div className="grid grid-cols-[minmax(0,1fr)_96px] items-start gap-m">
          <Field name="host" label="Host" disabled={disabled} validate={validateHost}>
            <Input placeholder="203.0.113.10" autoComplete="off" spellCheck={false} />
          </Field>
          <Field name="port" label="Port" disabled={disabled} validate={validatePort}>
            <Input defaultValue="22" inputMode="numeric" autoComplete="off" />
          </Field>
        </div>
        <div className="grid grid-cols-2 items-start gap-m">
          <Field name="user" label="User" description="Root, or a user with passwordless sudo." disabled={disabled}>
            <Input defaultValue="root" autoComplete="off" spellCheck={false} />
          </Field>
          <Field name="name" label="Name" disabled={disabled} validate={(input) => (String(input).trim() ? null : 'Enter a name.')}>
            <Input maxLength={64} autoComplete="off" spellCheck={false} />
          </Field>
        </div>
        <FormError error={ssh.error} />
      </Form>
    </>
  )
}

/** A failed request that is not about one field, for example a 503 when this runplane cannot add servers. */
function FormError({ error }: { error: unknown }) {
  if (!error || Object.keys(fieldErrors(error)).length > 0) return null
  return (
    <p role="alert" className="text-s text-error">
      {errorMessage(error)}
    </p>
  )
}

/** The host that new servers connect to: the public_url setting, or this page's host. */
function PublicHost() {
  const publicUrl = $api.useQuery('get', '/api/settings').data?.public_url
  return <>{publicUrl ? new URL(publicUrl).host : window.location.host}</>
}

function validateHost(value: unknown): string | null {
  return String(value).trim() ? null : 'Enter a host name or an IP address.'
}

function validatePort(value: unknown): string | null {
  const port = Number(value)
  return Number.isInteger(port) && port >= 1 && port <= 65535 ? null : 'Enter a port from 1 to 65535.'
}

/** True once the time has passed. */
function useExpired(time: string | undefined): boolean {
  const [expired, setExpired] = useState(false)
  useEffect(() => {
    if (!time) return
    const timer = setTimeout(() => setExpired(true), Math.max(0, Date.parse(time) - Date.now()))
    return () => {
      clearTimeout(timer)
      setExpired(false)
    }
  }, [time])
  return expired
}
