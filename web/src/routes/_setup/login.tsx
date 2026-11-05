import { createFileRoute, redirect } from '@tanstack/react-router'
import { currentSession } from '~/api/session'
import { LoginPage } from '~/components/pages/setup/login-page'
import { pageHead } from '~/lib/head'
import { safeRedirectPath } from '~/lib/redirect'

export const Route = createFileRoute('/_setup/login')({
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => ({
    redirect: typeof search.redirect === 'string' ? search.redirect : undefined,
  }),
  beforeLoad: async ({ context: { queryClient }, search }) => {
    if (await currentSession(queryClient)) throw redirect({ href: safeRedirectPath(search.redirect) })
  },
  head: () => pageHead('Log in'),
  component: () => <LoginPage redirect={Route.useSearch().redirect} />,
})
