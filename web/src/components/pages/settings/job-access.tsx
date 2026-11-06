import type { Schemas } from '~/api/client'
import { Switch } from '~/components/ui/switch'
import { publicReposSwitch } from './job-access-info'
import { ReadOnlyNote, SettingRows, SettingsSection } from './parts'
import { SwitchSetting } from './setting-controls'

/** The "Job access" switches of Settings · GitHub. */
export function JobAccess({ settings, admin }: { settings: Schemas['Settings']; admin: boolean }) {
  const publicRepos = publicReposSwitch(settings, admin)
  // Dev mode runs jobs in Docker containers too.
  const sandbox = settings.mode === 'vm' ? 'virtual machine' : 'container'

  return (
    <SettingsSection title="Job access">
      {!admin && <ReadOnlyNote>Only an admin can change job access.</ReadOnlyNote>}
      <SettingRows>
        <SwitchSetting
          name="allow_public_repos"
          label="Run jobs from public repositories"
          description={publicRepos.description}
          checked={settings.allow_public_repos}
          disabled={publicRepos.disabled}
        />
        <SwitchSetting
          name="allow_fork_prs"
          label="Run jobs from forked pull requests"
          description={
            settings.allow_public_repos
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
