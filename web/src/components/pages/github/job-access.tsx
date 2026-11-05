import { useQueryClient } from '@tanstack/react-query'
import { $api, errorMessage, type Schemas } from '~/api/client'
import { SectionHeader } from '~/components/app/section-header'
import { settingsQuery } from '~/components/pages/settings/settings-api'
import { Switch } from '~/components/ui/switch'
import { cx } from '~/lib/cx'

type Access = Partial<Pick<Schemas['Settings'], 'allow_public_repos' | 'allow_fork_prs'>>

/**
 * The "Job access" switches of Settings · GitHub. A switch moves at once and
 * moves back if the API refuses the change.
 */
export function JobAccess({ settings, canEdit }: { settings: Schemas['Settings']; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const update = $api.useMutation('patch', '/api/settings', {
    onMutate: async ({ body }) => {
      await queryClient.cancelQueries({ queryKey: settingsQuery.queryKey })
      const previous = queryClient.getQueryData(settingsQuery.queryKey)
      // `change` is the only caller, so the body holds only the two switches.
      queryClient.setQueryData(settingsQuery.queryKey, (old) => old && { ...old, ...(body as Access) })
      return { previous }
    },
    onError: (_error, _variables, context) => queryClient.setQueryData(settingsQuery.queryKey, context?.previous),
    onSuccess: (saved) => queryClient.setQueryData(settingsQuery.queryKey, saved),
  })
  const change = (access: Access) => update.mutate({ body: access })
  // The refused change shows under its own switch.
  const errorOf = (key: keyof Access) =>
    update.error && update.variables && key in update.variables.body ? errorMessage(update.error) : undefined

  // Container mode refuses public repositories whatever the setting says.
  const container = settings.mode === 'container'
  const publicRepos = settings.allow_public_repos && !container
  // Dev mode runs jobs in Docker containers too.
  const sandbox = settings.mode === 'vm' ? 'virtual machine' : 'container'

  return (
    <section className="flex flex-col gap-xs">
      <SectionHeader title="Job access" />
      {!canEdit && <p className="text-s text-neutral-7">Only an admin can change job access.</p>}
      {/* The divider under each row gets its own pixel of padding, as a border would. */}
      <div
        className={cx(
          'flex flex-col [&>*]:py-m',
          '[&>*:not(:last-child)]:pb-[calc(var(--space-m)+1px)] [&>*:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]',
        )}
      >
        <Switch
          label="Run jobs from public repositories"
          description={
            container
              ? 'Container mode always refuses jobs from public repositories. Switch to VM mode to run them.'
              : 'Anyone who opens a pull request could run code on your servers.'
          }
          checked={publicRepos}
          error={errorOf('allow_public_repos')}
          disabled={!canEdit || container}
          onCheckedChange={(checked) => change({ allow_public_repos: checked })}
        />
        <Switch
          label="Run jobs from forked pull requests"
          description={
            publicRepos
              ? 'Fork jobs never receive repository secrets.'
              : 'Applies to private repositories. Fork jobs never receive repository secrets.'
          }
          checked={settings.allow_fork_prs}
          error={errorOf('allow_fork_prs')}
          disabled={!canEdit}
          onCheckedChange={(checked) => change({ allow_fork_prs: checked })}
        />
        {/* Read-only, not disabled: the design shows it on, and it is a fact rather than an unavailable choice. */}
        <Switch
          label="Remove runners after each job"
          description={`Always on. Each job gets a new ${sandbox}, and its runner is removed when the job ends.`}
          checked
          readOnly
        />
      </div>
    </section>
  )
}
