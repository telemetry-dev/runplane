import { useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { githubQuery, statusQuery } from '~/api/queries'
import { CodeBlock } from '~/components/app/code-block'
import { ConnectedAccounts, JobWait, LabelTable, workflowDiff } from '~/components/pages/github/first-job'
import { Loading } from '~/components/pages/github/parts'
import { poolsQuery } from '~/components/pages/github/queries'
import { suggestedLabel } from '~/components/pages/overview/no-jobs'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_setup/setup/first-job')({
  loader: ({ context: { queryClient } }) =>
    Promise.all([queryClient.ensureQueryData(githubQuery), queryClient.ensureQueryData(poolsQuery)]),
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
  const { installations } = useSuspenseQuery(githubQuery).data
  const pools = useSuspenseQuery(poolsQuery).data
  const labels = pools.filter((pool) => pool.enabled)
  const label = suggestedLabel(pools)

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
      <LabelTable pools={labels} />
      <JobWait onJob={() => void openDashboard()} />
    </>
  )
}

function Header() {
  return (
    <header className="flex flex-col gap-s">
      <h1 className="type-display text-neutral-10">Run your first job</h1>
      <p className="text-m tracking-l text-neutral-8">
        Change one line in any workflow. Jobs with a runplane label run on your servers; everything else stays on GitHub.
      </p>
    </header>
  )
}