import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { $api, isApiError, type Schemas } from '~/api/client'
import { useFormProblems } from '~/api/form-problems'
import { inviteQuery } from '~/api/invites'
import { limits } from '~/api/limits'
import { sessionQuery } from '~/api/session'
import { FormAlert } from '~/components/app/form-alert'
import { LoadError } from '~/components/app/load-error'
import { roleWithArticle } from '~/components/pages/settings/member-info'
import { useElapsed } from '~/components/shell/pending'
import { SetupColumn } from '~/components/shell/setup'
import { Button, buttonClassName } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { Spinner } from '~/components/ui/spinner'
import { FormPanel, SetupHeader } from './parts'

/** The page of an invite link: who invited the visitor, and the form that makes their account. */
export function InvitePage({ token }: { token: string }) {
  // A query, not a loader, so that a used or expired invite gets its own page.
  const invite = useQuery(inviteQuery(token))

  if (invite.isPending) return <InviteLoading />
  if (isApiError(invite.error, 404)) return <InvalidInvite />
  if (invite.isError) {
    return (
      <SetupColumn>
        <SetupHeader title={joinTitle()} />
        <LoadError what="the invite" error={invite.error} onRetry={() => void invite.refetch()} retrying={invite.isFetching} />
      </SetupColumn>
    )
  }
  return <AcceptForm token={token} invite={invite.data} />
}

const fields = ['name', 'password'] as const
const acceptLimits = limits.AcceptInviteRequest

function AcceptForm({ token, invite }: { token: string; invite: Schemas['InviteInfo'] }) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const accept = $api.useMutation('post', '/api/invites/{id}', {
    onSuccess: async (user) => {
      queryClient.setQueryData(sessionQuery.queryKey, user)
      await navigate({ to: '/' })
    },
  })
  const { fields: errors, alert } = useFormProblems(accept.error, { fields })

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
        <span className="font-medium text-neutral-10">{roleWithArticle(invite.role)}</span>.
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
            <Input autoComplete="name" maxLength={acceptLimits.name.maxLength} required autoFocus />
          </Field>
          <Field name="password" label="Password" description={`At least ${acceptLimits.password.minLength} characters.`}>
            <Input type="password" autoComplete="new-password" minLength={acceptLimits.password.minLength} required />
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
  const slow = useElapsed(300)
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
