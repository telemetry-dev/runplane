import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useRef } from 'react'
import { $api } from '~/api/client'
import { useFormProblems } from '~/api/form-problems'
import { sessionQuery } from '~/api/session'
import { FormAlert } from '~/components/app/form-alert'
import { SetupColumn } from '~/components/shell/setup'
import { Button } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { safeRedirectPath } from '~/lib/redirect'
import { FormPanel, SetupHeader } from './parts'

const fields = ['email', 'password'] as const

/** Log in. After it, the browser opens `redirect`, the page that asked for the session. */
export function LoginPage({ redirect }: { redirect: string | undefined }) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const password = useRef<HTMLInputElement>(null)
  const login = $api.useMutation('post', '/api/session', {
    onSuccess: async (user) => {
      queryClient.setQueryData(sessionQuery.queryKey, user)
      const path = safeRedirectPath(redirect)
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
  const { fields: errors, alert } = useFormProblems(login.error, { fields })

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
