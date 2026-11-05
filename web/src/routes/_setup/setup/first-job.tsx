import { useSuspenseQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { githubQuery } from '~/api/github'
import { poolsQuery } from '~/api/pools'
import { CodeBlock } from '~/components/app/code-block'
import { ConnectedAccounts, JobWait, LabelTable, workflowDiff } from '~/components/pages/github/first-job'
import { Loading } from '~/components/pages/github/parts'
import { suggestedLabel } from '~/components/pages/overview/no-jobs'
import { SetupHeader } from '~/components/pages/setup/parts'
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
  const navigate = useNavigate()
  const { installations } = useSuspenseQuery(githubQuery).data
  const pools = useSuspenseQuery(poolsQuery).data
  const labels = pools.filter((pool) => pool.enabled)
  const label = suggestedLabel(pools)

  return (
    <>
      <Header />
      <ConnectedAccounts installations={installations} />
      {label && <CodeBlock title=".github/workflows/ci.yml" code={workflowDiff(label)} copyable />}
      <LabelTable pools={labels} />
      <JobWait onJob={() => void navigate({ to: '/' })} />
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