import type { Schemas } from '~/api/client'
import { publicReposRun } from '~/api/settings'
import { Switch } from '~/components/ui/switch'
import { ReadOnlyNote, SettingRows, SettingsSection } from './parts'
import { SwitchSetting } from './setting-controls'

/** The "Job access" switches of Settings · GitHub. */
export function JobAccess({ settings, admin }: { settings: Schemas['Settings']; admin: boolean }) {
  // The setting has no effect while jobs of public repositories cannot run.
  const publicRepos = publicReposRun(settings)
  // Dev mode runs jobs in Docker containers too.
  const sandbox = settings.mode === 'vm' ? 'virtual machine' : 'container'

  return (
    <SettingsSection title="Job access">
      {!admin && <ReadOnlyNote>Only an admin can change job access.</ReadOnlyNote>}
      <SettingRows>
        <SwitchSetting
          name="allow_public_repos"
          label="Run jobs from public repositories"
          description={settings.public_repos_refusal || 'Anyone who opens a pull request could run code on your servers.'}
          checked={publicRepos}
          disabled={!admin || !settings.public_repos_possible}
        />
        <SwitchSetting
          name="allow_fork_prs"
          label="Run jobs from forked pull requests"
          description={
            publicRepos
              ? 'Fork jobs never receive repository secrets.'
              : 'Applies to private repositories. Fork jobs never receive repository secrets.'
          }
          checked={settings.allow_fork_prs}
          disabled={!admin}
        />
        {/* Read-only, not disabled: the design shows it on, and it is a fact rather than an unavailable choice. */}
        <Switch
          label="Remove runners after each job"
          description={`Always on. Each job gets a new ${sandbox}, and its runner is removed when the job ends.`}
          checked
          readOnly
          className="py-m"
        />
      </SettingRows>
    </SettingsSection>
  )
}
