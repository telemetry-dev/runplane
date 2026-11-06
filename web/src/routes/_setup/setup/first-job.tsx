import { useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { githubQuery, statusQuery } from '~/api/queries'
import { CodeBlock } from '~/components/app/code-block'
import { ConnectedAccounts, JobWait, LabelTable, workflowDiff } from '~/components/pages/github/first-job'
import { jobRepositories } from '~/components/pages/github/links'
import { Loading } from '~/components/pages/github/parts'
import { PullRequestForm } from '~/components/pages/github/pull-request-form'
import { suggestedLabel } from '~/components/pages/overview/no-jobs'
import { poolsQuery } from '~/components/pages/runners/queries'
import { settingsQuery, useIsAdmin } from '~/components/pages/settings/settings-api'
import { SetupHeader } from '~/components/pages/setup/parts'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/setup/first-job')({
  loader: ({ context: { queryClient } }) =>
    Promise.all([
      queryClient.ensureQueryData(githubQuery),
      queryClient.ensureQueryData(poolsQuery),
      queryClient.ensureQueryData(settingsQuery),
    ]),
  head: () => pageHead('First job'),
  pendingComponent: () => (
    <>
      <Header />
      <Loading />
    </>
  ),
  component: FirstJobStep,
})

function FirstJobStep() {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const admin = useIsAdmin()
  const { app, installations } = useSuspenseQuery(githubQuery).data
  const pools = useSuspenseQuery(poolsQuery).data
  const settings = useSuspenseQuery(settingsQuery).data
  const labels = pools.filter((pool) => pool.enabled)
  const label = suggestedLabel(pools)
  const repositories = jobRepositories(installations, settings)

  async function openDashboard() {
    // The overview reads has_jobs from the status to leave its empty state.
    await queryClient.invalidateQueries({ queryKey: statusQuery.queryKey })
    await navigate({ to: '/' })
  }

  return (
    <>
      <Header />
      <ConnectedAccounts installations={installations} />
      {label && <CodeBlock title=".github/workflows/ci.yml" code={workflowDiff(label)} copyable />}
      {label && admin && repositories.length > 0 && (
        <section className="flex flex-col gap-xs">
          <h2 className="text-s font-medium text-neutral-10">Open a pull request</h2>
          <PullRequestForm
            repositories={repositories}
            labels={[label]}
            defaultLabel={label}
            permissionsUrl={app && `${app.settings_url}/permissions`}
          />
        </section>
      )}
      <LabelTable pools={labels} />
      <JobWait onJob={() => void openDashboard()} />
    </>
  )
}

function Header() {
  return (
    <SetupHeader title="Run your first job">
      Change one line in any workflow. Jobs with a runplane label run on your servers; everything else stays on GitHub.
    </SetupHeader>
  )
}
