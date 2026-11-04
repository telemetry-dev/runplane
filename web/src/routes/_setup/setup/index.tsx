import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useEffect, useEffectEvent, useMemo, useState } from 'react'
import { $api, isApiError } from '~/api/client'
import { sessionQuery, statusQuery } from '~/api/queries'
import { HostCheck, useHostCheck } from '~/components/pages/setup/host-check'
import { FormAlert, FormPanel, SetupHeader } from '~/components/pages/setup/parts'
import { submitProblems } from '~/components/pages/setup/problems'
import { Button } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/setup/')({
  head: () => pageHead('Create your account'),
  component: ServerStep,
})

const fields = ['code', 'name', 'email', 'password'] as const

function ServerStep() {
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
    const problems = submitProblems(create.error, fields)
    if (host.codeError) problems.errors = { code: host.codeError, ...problems.errors }
    return problems
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
          create.mutate({ body: { code: values.code, name: values.name, email: values.email, password: values.password } })
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
            <Input autoComplete="name" maxLength={100} required />
          </Field>
          <Field name="email" label="Email">
            <Input type="email" autoComplete="username" required />
          </Field>
          <Field name="password" label="Password" description="At least 10 characters.">
            <Input type="password" autoComplete="new-password" minLength={10} required />
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
