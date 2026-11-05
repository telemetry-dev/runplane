import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, redirect, useNavigate } from '@tanstack/react-router'
import { useMemo, useRef } from 'react'
import { $api } from '~/api/client'
import { sessionQuery, statusQuery } from '~/api/queries'
import { FormAlert, FormPanel, SetupHeader } from '~/components/pages/setup/parts'
import { submitProblems } from '~/components/pages/setup/problems'
import { SetupColumn } from '~/components/shell/setup'
import { Button } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { pageHead } from '~/lib/head'
import { safeRedirectPath } from '~/lib/redirect'

export const Route = createFileRoute('/_setup/login')({
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => ({
    redirect: typeof search.redirect === 'string' ? search.redirect : undefined,
  }),
  beforeLoad: async ({ context: { queryClient }, search }) => {
    const status = await queryClient.ensureQueryData(statusQuery)
    if (status.needs_setup) throw redirect({ to: '/setup' })
    const user = await queryClient.ensureQueryData(sessionQuery)
    if (user) throw redirect({ href: safeRedirectPath(search.redirect) })
  },
  head: () => pageHead('Log in'),
  component: LoginPage,
})

const fields = ['email', 'password'] as const

function LoginPage() {
  const search = Route.useSearch()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const password = useRef<HTMLInputElement>(null)
  const login = $api.useMutation('post', '/api/session', {
    onSuccess: async (user) => {
      queryClient.setQueryData(sessionQuery.queryKey, user)
      const path = safeRedirectPath(search.redirect)
      // The GitHub callbacks are API routes, which the router cannot open.
      if (path.startsWith('/api/')) window.location.assign(path)
      else await navigate({ href: path })
    },
    // Both values stay, and the selected password can be typed over at once.
    onError: () => {
      password.current?.focus()
      password.current?.select()
    },
  })
  const { errors, alert } = useMemo(() => submitProblems(login.error, fields), [login.error])

  return (
    <SetupColumn>
      <SetupHeader title="Log in" />
      <Form
        className="gap-xl"
        errors={errors}
        onChange={() => {
          if (login.isError) login.reset()
        }}
        onFormSubmit={(values: Record<(typeof fields)[number], string>) =>
          login.mutate({ body: { email: values.email, password: values.password } })
        }
      >
        <FormPanel>
          <Field name="email" label="Email">
            <Input type="email" autoComplete="username" required autoFocus />
          </Field>
          <Field name="password" label="Password">
            <Input ref={password} type="password" autoComplete="current-password" required />
          </Field>
          {alert && <FormAlert>{alert}</FormAlert>}
        </FormPanel>
        <Button type="submit" variant="primary" loading={login.isPending} className="self-start">
          Log in
        </Button>
      </Form>
    </SetupColumn>
  )
}
