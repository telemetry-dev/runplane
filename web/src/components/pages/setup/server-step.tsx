import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useEffect, useEffectEvent, useMemo, useState } from 'react'
import { $api, isApiError } from '~/api/client'
import { formProblems } from '~/api/form-problems'
import { limits } from '~/api/limits'
import { sessionQuery, statusQuery } from '~/api/session'
import { Button } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { HostCheck, useHostCheck } from './host-check'
import { FormAlert, FormPanel, SetupHeader } from './parts'

const fields = ['code', 'name', 'email', 'password'] as const
const setupLimits = limits.SetupRequest

/** Setup step 1: the setup code from the install script, the first admin account, and the check of this server. */
export function ServerStep() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [code, setCode] = useState('')
  const host = useHostCheck(code)

  // The guards read the cached status, so refresh it before the next page.
  const refreshStatus = () => queryClient.invalidateQueries({ queryKey: statusQuery.queryKey, refetchType: 'all' })

  const create = $api.useMutation('post', '/api/setup', {
    onSuccess: async (user) => {
      queryClient.setQueryData(sessionQuery.queryKey, user)
      await refreshStatus()
      await navigate({ to: '/setup/github' })
    },
  })

  // The first admin exists already, for example from another browser.
  const setupDone = isApiError(create.error, 409) || isApiError(host.query.error, 409)
  const logIn = useEffectEvent(async () => {
    await refreshStatus()
    await navigate({ to: '/login' })
  })
  useEffect(() => {
    if (setupDone) void logIn()
  }, [setupDone])

  // Problems from the server refer to the values sent, so an edit clears them
  // (see onChange below).
  const { errors, alert } = useMemo(() => {
    const problems = formProblems(create.error, { fields })
    return { errors: host.codeError ? { code: host.codeError, ...problems.fields } : problems.fields, alert: problems.alert }
  }, [create.error, host.codeError])

  return (
    <>
      <SetupHeader title="Create your account">The first account is the admin. You can invite your team after the setup.</SetupHeader>
      <Form
        className="gap-xl"
        errors={errors}
        onChange={() => {
          if (create.isError) create.reset()
        }}
        onFormSubmit={(values: Record<(typeof fields)[number], string>) =>
          create.mutate({ body: { code: values.code.trim(), name: values.name, email: values.email, password: values.password } })
        }
      >
        <FormPanel>
          <Field
            name="code"
            label="Setup code"
            // The hint fits on one line at the width of the field.
            wide
            description={
              <>
                The install script printed it. For a new code, run{' '}
                <code className="font-code text-xs leading-s tracking-m text-neutral-8">sudo runplane setup-code</code>.
              </>
            }
          >
            <Input
              onValueChange={setCode}
              placeholder="XXXX-XXXX-XXXX"
              autoComplete="one-time-code"
              autoCapitalize="characters"
              spellCheck={false}
              autoFocus
              required
              className="font-code tracking-xl uppercase"
            />
          </Field>
          <Field name="name" label="Name">
            <Input autoComplete="name" maxLength={setupLimits.name.maxLength} required />
          </Field>
          <Field name="email" label="Email">
            <Input type="email" autoComplete="username" required />
          </Field>
          <Field name="password" label="Password" description={`At least ${setupLimits.password.minLength} characters.`}>
            <Input type="password" autoComplete="new-password" minLength={setupLimits.password.minLength} required />
          </Field>
          {alert && <FormAlert>{alert}</FormAlert>}
        </FormPanel>
        <HostCheck query={host.query} ready={host.ready} />
        <Button type="submit" variant="primary" loading={create.isPending} className="self-start">
          Create account
        </Button>
      </Form>
    </>
  )
}
