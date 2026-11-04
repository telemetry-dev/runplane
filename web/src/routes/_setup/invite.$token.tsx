import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useEffect, useMemo, useState } from 'react'
import { $api, errorMessage, isApiError, type Schemas } from '~/api/client'
import { sessionQuery } from '~/api/queries'
import { FormAlert, FormPanel, SetupHeader } from '~/components/pages/setup/parts'
import { submitProblems } from '~/components/pages/setup/problems'
import { SetupColumn } from '~/components/shell/setup'
import { Button, buttonClassName } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { Spinner } from '~/components/ui/spinner'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/invite/$token')({
  head: () => pageHead('Join'),
  component: InvitePage,
})

function InvitePage() {
  const { token } = Route.useParams()
  // A query, not a loader, so that a used or expired invite gets its own page.
  const invite = $api.useQuery('get', '/api/invites/{id}', { params: { path: { id: token } } }, { staleTime: Infinity })

  if (invite.isPending) return <InviteLoading />
  if (isApiError(invite.error, 404)) return <InvalidInvite />
  if (invite.isError) return <InviteLoadError error={invite.error} retrying={invite.isFetching} onRetry={() => void invite.refetch()} />
  return <AcceptForm token={token} invite={invite.data} />
}

const fields = ['name', 'password'] as const

function AcceptForm({ token, invite }: { token: string; invite: Schemas['InviteInfo'] }) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const accept = $api.useMutation('post', '/api/invites/{id}', {
    onSuccess: async (user) => {
      queryClient.setQueryData(sessionQuery.queryKey, user)
      await navigate({ to: '/' })
    },
  })
  const { errors, alert } = useMemo(() => submitProblems(accept.error, fields), [accept.error])

  // Used or expired since the page opened.
  if (isApiError(accept.error, 404)) return <InvalidInvite />

  return (
    <SetupColumn>
      <SetupHeader title={joinTitle()}>
        {invite.invited_by ? (
          <>
            <span className="font-medium text-neutral-10">{invite.invited_by}</span> invited you as
          </>
        ) : (
          'You are invited as'
        )}{' '}
        {invite.role === 'admin' ? 'an' : 'a'} <span className="font-medium text-neutral-10">{invite.role}</span>.
      </SetupHeader>
      <Form
        className="gap-xl"
        errors={errors}
        onChange={() => {
          if (accept.isError) accept.reset()
        }}
        onFormSubmit={(values: Record<(typeof fields)[number], string>) =>
          accept.mutate({ params: { path: { id: token } }, body: { name: values.name, password: values.password } })
        }
      >
        <FormPanel>
          <dl className="flex flex-col gap-xxs text-s">
            <dt className="font-medium text-neutral-10">Email</dt>
            <dd className="text-neutral-8">{invite.email}</dd>
          </dl>
          {/* Password managers save the new password under this email. */}
          <input name="username" autoComplete="username" value={invite.email} readOnly hidden />
          <Field name="name" label="Name">
            <Input autoComplete="name" maxLength={100} required autoFocus />
          </Field>
          <Field name="password" label="Password" description="At least 10 characters.">
            <Input type="password" autoComplete="new-password" minLength={10} required />
          </Field>
          {alert && <FormAlert>{alert}</FormAlert>}
        </FormPanel>
        <Button type="submit" variant="primary" loading={accept.isPending} className="self-start">
          Join
        </Button>
      </Form>
    </SetupColumn>
  )
}

function InviteLoading() {
  // Most invites load at once; the spinner shows only when it takes a while.
  const [slow, setSlow] = useState(false)
  useEffect(() => {
    const timer = setTimeout(() => setSlow(true), 300)
    return () => clearTimeout(timer)
  }, [])
  return (
    <SetupColumn>
      <SetupHeader title={joinTitle()} />
      {slow && (
        <p role="status" className="flex items-center gap-s text-s font-medium text-neutral-10">
          <Spinner size={16} />
          Loading the invite
        </p>
      )}
    </SetupColumn>
  )
}

function InviteLoadError({ error, retrying, onRetry }: { error: unknown; retrying: boolean; onRetry: () => void }) {
  return (
    <SetupColumn>
      <SetupHeader title={joinTitle()}>Could not load the invite. {errorMessage(error)}</SetupHeader>
      <Button loading={retrying} onClick={onRetry} className="self-start">
        Try again
      </Button>
    </SetupColumn>
  )
}

function InvalidInvite() {
  return (
    <SetupColumn>
      <SetupHeader title="This invite does not work">
        It is wrong, used, or expired. Ask an admin of {location.host} for a new invite.
      </SetupHeader>
      <Link to="/login" className={buttonClassName('secondary', 'self-start')}>
        Log in
      </Link>
    </SetupColumn>
  )
}

function joinTitle() {
  return `Join ${location.host}`
}
