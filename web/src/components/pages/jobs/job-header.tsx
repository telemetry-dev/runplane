import { useQuery } from '@tanstack/react-query'
import { RotateCcw, TerminalIcon } from 'lucide-react'
import { $api, errorMessage, type Schemas } from '~/api/client'
import { BreadcrumbLink, BreadcrumbPage, Breadcrumbs } from '~/components/app/breadcrumbs'
import { jobStatus, StatusIcon } from '~/components/app/status-icon'
import { Button, buttonClassName } from '~/components/ui/button'
import { GithubIcon } from '~/components/ui/icons'
import { toastManager } from '~/components/ui/toast'
import { runNumber, workflowFile } from './job-format'
import { settingsQuery } from './job-queries'

/** Breadcrumbs, the job name with its status, and the GitHub and re-run actions. */
export function JobHeader({ job }: { job: Schemas['Job'] }) {
  const holdMinutes = useQuery(settingsQuery).data?.debug_hold_minutes
  const rerun = $api.useMutation('post', '/api/jobs/{id}/rerun', {
    onSuccess: (_, { body }) => {
      const attempt = `Attempt ${job.run_attempt + 1} shows in Jobs when GitHub queues it.`
      const hold = holdMinutes ? ` Its sandbox stays ${holdMinutes} minutes after the job for a shell.` : ''
      toastManager.add(
        body?.debug
          ? { title: 'Re-run with SSH requested', description: attempt + hold }
          : { title: 'Re-run requested', description: attempt },
      )
    },
    onError: (error) => toastManager.add({ title: 'Could not re-run the job', description: errorMessage(error) }),
  })
  const pending = rerun.isPending ? (rerun.variables.body?.debug ? 'debug' : 'plain') : null

  function start(debug: boolean) {
    rerun.mutate({ params: { path: { id: job.id } }, body: { debug } })
  }

  return (
    <header className="flex flex-col gap-s">
      <Breadcrumbs>
        <BreadcrumbLink to="/jobs">Jobs</BreadcrumbLink>
        <BreadcrumbLink to="/jobs" search={{ repository: job.repository }}>
          {job.repository}
        </BreadcrumbLink>
        <BreadcrumbPage>{[workflowFile(job.workflow_file), runNumber(job)].filter(Boolean).join(' ')}</BreadcrumbPage>
      </Breadcrumbs>
      <div className="flex flex-wrap items-center justify-between gap-m">
        <div className="flex min-w-0 items-center gap-s">
          <StatusIcon status={jobStatus(job)} size={22} />
          <h1 className="type-page-title truncate text-neutral-10">{job.job_name}</h1>
        </div>
        <div className="flex flex-wrap items-center gap-xs">
          {/* Before runplane reads the job from GitHub, the URL is the page of its run; a job without a run ID has none. */}
          {job.html_url && (
            <a href={job.html_url} target="_blank" rel="noreferrer" className={buttonClassName('secondary')}>
              <GithubIcon aria-hidden />
              View on GitHub
            </a>
          )}
          {/* GitHub re-runs only a completed job. */}
          {job.state === 'completed' && (
            <>
              <Button loading={pending === 'debug'} disabled={pending === 'plain'} onClick={() => start(true)}>
                {pending !== 'debug' && <TerminalIcon aria-hidden />}
                Re-run with SSH
              </Button>
              <Button variant="primary" loading={pending === 'plain'} disabled={pending === 'debug'} onClick={() => start(false)}>
                {pending !== 'plain' && <RotateCcw aria-hidden />}
                Re-run job
              </Button>
            </>
          )}
        </div>
      </div>
    </header>
  )
}
