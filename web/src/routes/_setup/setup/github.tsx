import { useSuspenseQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { githubQuery } from '~/api/queries'
import { ConnectForm } from '~/components/pages/github/connect-form'
import { callbackSearch } from '~/components/pages/github/callback'
import { ExistingApp } from '~/components/pages/github/existing-app'
import { CallbackError, InstallRequested, Loading } from '~/components/pages/github/parts'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/setup/github')({
  validateSearch: callbackSearch,
  loader: ({ context: { queryClient } }) => queryClient.ensureQueryData(githubQuery),
  head: () => pageHead('Connect GitHub'),
  pendingComponent: () => (
    <>
      <Header />
      <Loading />
    </>
  ),
  component: GithubStep,
})

function GithubStep() {
  const { error, requested } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { app, installations, can_sign_in } = useSuspenseQuery(githubQuery).data
  const dismiss = () => void navigate({ search: {}, replace: true })

  return (
    <>
      <Header />
      {error && <CallbackError message={error} onDismiss={dismiss} />}
      {requested && <InstallRequested installations={installations.length} canSync onDismiss={dismiss} />}
      {app ? <ExistingApp app={app} installations={installations} /> : <ConnectForm canSignIn={can_sign_in} />}
    </>
  )
}

function Header() {
  return (
    <header className="flex flex-col gap-s">
      <h1 className="type-display text-neutral-10">Connect GitHub</h1>
      <p className="text-m tracking-l text-neutral-8">
        runplane creates a GitHub App for this instance. When a workflow job asks for a runplane label, runplane starts a fresh
        runner for it on one of your servers.
      </p>
    </header>
  )
}
