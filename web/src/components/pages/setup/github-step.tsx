import { useSuspenseQuery, type QueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { githubQuery } from '~/api/github'
import type { CallbackSearch } from '~/components/pages/github/callback'
import { ConnectForm } from '~/components/pages/github/connect-form'
import { ExistingApp } from '~/components/pages/github/existing-app'
import { CallbackError, InstallRequested, Loading } from '~/components/pages/github/parts'
import { SetupHeader } from './parts'

export function prefetch(client: QueryClient) {
  return client.ensureQueryData(githubQuery)
}

/** Setup step 2: make the GitHub App and install it. `search` is what the GitHub callbacks report. */
export function GitHubStep({ search: { error, requested } }: { search: CallbackSearch }) {
  const navigate = useNavigate()
  const { app, installations, app_permissions } = useSuspenseQuery(githubQuery).data
  const dismiss = () => void navigate({ to: '/setup/github', search: {}, replace: true })

  return (
    <>
      <Header />
      {error && <CallbackError message={error} onDismiss={dismiss} />}
      {requested && <InstallRequested installations={installations.length} canSync onDismiss={dismiss} />}
      {app ? <ExistingApp app={app} installations={installations} /> : <ConnectForm permissions={app_permissions} />}
    </>
  )
}

export function GitHubStepLoading() {
  return (
    <>
      <Header />
      <Loading />
    </>
  )
}

function Header() {
  return (
    <SetupHeader title="Connect GitHub">
      runplane creates a GitHub App for this instance. When a workflow job asks for a runplane label, runplane starts a fresh runner
      for it on one of your servers.
    </SetupHeader>
  )
}
