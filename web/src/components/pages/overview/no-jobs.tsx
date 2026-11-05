import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { errorMessage, type Schemas } from '~/api/client'
import { poolsQuery } from '~/api/pools'
import { CodeBlock, type CodeLine } from '~/components/app/code-block'
import { CopyButton } from '~/components/app/copy-button'
import { EmptyState } from '~/components/app/empty-state'
import { Button, buttonClassName } from '~/components/ui/button'

/** The "No jobs yet" panel: the runs-on change that sends a first job to runplane. */
export function NoJobs() {
  const { data: pools, isError, error, refetch, isFetching } = useQuery(poolsQuery)
  const label = pools && suggestedLabel(pools)

  if (isError && !pools) {
    return (
      <EmptyState
        title="No jobs yet"
        actions={
          <>
            <Button variant="primary" loading={isFetching} onClick={() => void refetch()}>
              Try again
            </Button>
            <Link to="/runners" className={buttonClassName()}>
              See all labels
            </Link>
          </>
        }
      >
        Change runs-on in any workflow to send that job to your servers.{' '}
        <span role="alert">Could not load your runner labels. {errorMessage(error)}</span>
      </EmptyState>
    )
  }

  if (pools && !label) {
    return (
      <EmptyState
        title="No jobs yet"
        actions={
          <Link to="/runners" className={buttonClassName('primary')}>
            Add a runner label
          </Link>
        }
      >
        Add a runner label, then use it in runs-on of a workflow to send that job to your servers.
      </EmptyState>
    )
  }
  return (
    <EmptyState
      title="No jobs yet"
      actions={
        label && (
          <>
            <CopyButton text={label} variant="primary">
              Copy label
            </CopyButton>
            <Link to="/runners" className={buttonClassName()}>
              See all labels
            </Link>
          </>
        )
      }
      aside={label && <CodeBlock title=".github/workflows/ci.yml" code={workflow(label)} />}
    >
      Change runs-on in any workflow to send that job to your servers. Jobs show up here as soon as GitHub queues them.
    </EmptyState>
  )
}

/** The label to suggest for a first job. A 4 vCPU x64 pool is at least as large as GitHub's ubuntu-latest runner, so it is a safe first change. */
export function suggestedLabel(pools: readonly Schemas['Pool'][]): string | undefined {
  const enabled = pools.filter((pool) => pool.enabled)
  return (enabled.find((pool) => pool.arch === 'x64' && pool.vcpu === 4) ?? enabled[0])?.label
}

function workflow(label: string): CodeLine[] {
  return [
    'jobs:',
    '  test:',
    { text: '    runs-on: ubuntu-latest', change: 'removed' },
    { text: `    runs-on: ${label}`, change: 'added' },
    '    steps:',
  ]
}
