import { useQuery } from '@tanstack/react-query'
import type { Schemas } from '~/api/client'
import { cachesQuery } from '~/api/caches'
import { statusQuery, useIsAdmin } from '~/api/session'
import { settingsQuery } from '~/api/settings'
import { formatBytes } from '~/lib/format'
import { Images } from './images'
import { LoadError, ReadOnlyNote, SettingRow, SettingRows, SettingsColumn, SettingsSection, SettingRowsSkeleton } from './parts'
import { RunnerVersions } from './runner-versions'
import { ListSetting, NumberSetting, SwitchSetting, useNumberSetting } from './setting-controls'

type Settings = Schemas['Settings']

/** Settings · Runners: capacity, job limits, images, runner versions, network, and caches. */
export function RunnerSettings() {
  const admin = useIsAdmin()
  const { data: settings, isError, refetch, isFetching } = useQuery(settingsQuery)
  const disabled = !admin

  return (
    <SettingsColumn>
      {!admin && <ReadOnlyNote />}
      {settings ? (
        <>
          <Capacity settings={settings} disabled={disabled} />
          <SettingsSection title="Jobs">
            <SettingRows>
              <NumberSetting
                label="Default time limit"
                description="For runner labels without their own limit. Longer jobs are stopped."
                value={settings.default_time_limit_minutes}
                toPatch={(default_time_limit_minutes) => ({ default_time_limit_minutes })}
                unit="min"
                min={1}
                max={7200}
                disabled={disabled}
              />
              <NumberSetting
                label="Debug hold"
                description="After a job re-run with SSH, its sandbox stays this long for the debug shell."
                value={settings.debug_hold_minutes}
                toPatch={(debug_hold_minutes) => ({ debug_hold_minutes })}
                unit="min"
                min={1}
                max={1440}
                disabled={disabled}
              />
            </SettingRows>
          </SettingsSection>
        </>
      ) : isError ? (
        <LoadError onRetry={() => void refetch()} retrying={isFetching} />
      ) : (
        <>
          <SettingsSection title="Capacity">
            <SettingRowsSkeleton count={3} control="number" />
          </SettingsSection>
          <SettingsSection title="Jobs">
            <SettingRowsSkeleton control="number" />
          </SettingsSection>
        </>
      )}

      <Images admin={admin} />
      <RunnerVersions />

      {settings && (
        <>
          <Network settings={settings} disabled={disabled} />
          <Caches settings={settings} disabled={disabled} />
        </>
      )}
    </SettingsColumn>
  )
}

function Capacity({ settings, disabled }: { settings: Settings; disabled: boolean }) {
  const vcpu = useNumberSetting({
    value: settings.reserve_vcpu,
    toPatch: (reserve_vcpu) => ({ reserve_vcpu }),
    unit: 'vCPU',
    min: 0,
    max: 256,
    disabled,
    'aria-label': 'vCPUs',
  })
  // The API keeps megabytes; the field shows gigabytes in steps of half a gigabyte.
  const memory = useNumberSetting({
    value: settings.reserve_memory_mb / 1024,
    toPatch: (gb) => ({ reserve_memory_mb: Math.round(gb * 1024) }),
    unit: 'GB memory',
    min: 0,
    max: 1024,
    step: 0.5,
    format: { maximumFractionDigits: 1 },
    disabled,
    'aria-label': 'GB memory',
  })

  return (
    <SettingsSection title="Capacity">
      <SettingRows>
        <SettingRow group label="Host reserve" description="Kept free for the system on each server." error={vcpu.error ?? memory.error}>
          <div className="flex gap-xs">
            {vcpu.field}
            {memory.field}
          </div>
        </SettingRow>
        <NumberSetting
          label="CPU overcommit"
          description="Sandboxes can get this many times the vCPUs of a server. 1× is no overcommit."
          value={settings.cpu_overcommit}
          toPatch={(cpu_overcommit) => ({ cpu_overcommit })}
          unit="×"
          min={0.5}
          max={8}
          step={0.25}
          format={{ minimumFractionDigits: 1, maximumFractionDigits: 2 }}
          disabled={disabled}
        />
        <NumberSetting
          label="Minimum free disk"
          description="No new sandboxes start on a server with less free disk."
          value={settings.min_free_disk_percent}
          toPatch={(min_free_disk_percent) => ({ min_free_disk_percent })}
          unit="%"
          min={0}
          max={90}
          disabled={disabled}
        />
      </SettingRows>
    </SettingsSection>
  )
}

function Network({ settings, disabled }: { settings: Settings; disabled: boolean }) {
  const mode = useQuery(statusQuery).data?.mode
  const container = (settings.mode || mode) === 'container'

  return (
    <SettingsSection title="Network">
      <SettingRows>
        <ListSetting
          label="DNS servers"
          description="Up to 4 IPv4 addresses, separated by commas."
          value={settings.dns_servers}
          toPatch={(dns_servers) => ({ dns_servers })}
          check={(items) =>
            items.length === 0 ? 'Enter at least one DNS server.' : items.length > 4 ? 'Enter at most 4 DNS servers.' : undefined
          }
          disabled={disabled}
        />
        <ListSetting
          lines
          label="Egress allowlist"
          description="Host names, *.domains, IP addresses, or CIDRs that sandboxes can reach, one on each line."
          value={settings.egress_allowlist}
          toPatch={(egress_allowlist) => ({ egress_allowlist })}
          placeholder="Empty: all of the public internet"
          disabled={disabled}
        />
        <NumberSetting
          label="Bandwidth limit"
          description="For each VM sandbox. VM mode only."
          value={settings.bandwidth_mbps}
          toPatch={(bandwidth_mbps) => ({ bandwidth_mbps })}
          unit="Mbit/s"
          min={0}
          max={100_000}
          zeroPlaceholder="No limit"
          className="w-[160px]"
          disabled={disabled || container}
        />
      </SettingRows>
    </SettingsSection>
  )
}

function Caches({ settings, disabled }: { settings: Settings; disabled: boolean }) {
  const used = useQuery(cachesQuery).data?.size_bytes
  return (
    <SettingsSection title="Caches">
      <SettingRows>
        <SwitchSetting
          name="actions_cache"
          label="Actions cache"
          description="actions/cache saves and restores on this instance instead of GitHub."
          checked={settings.actions_cache}
          disabled={disabled}
        />
        <SwitchSetting
          name="registry_mirror"
          label="Registry mirror"
          description="Docker Hub pulls in sandboxes go through a cache on this instance."
          checked={settings.registry_mirror}
          disabled={disabled}
        />
        <NumberSetting
          label="Cache size limit"
          description={`${used === undefined ? '' : `${formatBytes(used)} in use. `}Above the limit, the least recently used entries go first.`}
          value={settings.cache_max_gb}
          toPatch={(cache_max_gb) => ({ cache_max_gb })}
          unit="GB"
          min={1}
          max={100_000}
          disabled={disabled}
        />
      </SettingRows>
    </SettingsSection>
  )
}
