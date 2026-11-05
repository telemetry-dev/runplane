import { useQuery, type QueryClient } from '@tanstack/react-query'
import { Check, X } from 'lucide-react'
import type { Schemas } from '~/api/client'
import { useIsAdmin } from '~/api/session'
import { limits } from '~/api/limits'
import { domainDNSQuery, settingsQuery } from '~/api/settings'
import { LoadError } from '~/components/app/load-error'
import { Spinner } from '~/components/ui/spinner'
import { formatDate } from '~/lib/format'
import { Backups } from './backups'
import { dnsStatus, serverRecord } from './dns-info'
import { ReadOnlyNote, SettingRow, SettingRows, SettingsColumn, SettingsSection, SettingRowsSkeleton } from './parts'
import { NumberSetting, TextSetting } from './setting-controls'
import { Updates } from './updates'

/** Starts the request of the page; its sections show their own loading and error states. */
export function prefetch(client: QueryClient) {
  void client.prefetchQuery(settingsQuery)
}

/** Settings · General: instance, HTTPS, data retention, and for admins updates and backups. */
export function GeneralSettings() {
  const admin = useIsAdmin()
  const { data: settings, error, refetch, isFetching } = useQuery({
    ...settingsQuery,
    // No event reports the certificate, so ask until the request ends.
    refetchInterval: (query) => (query.state.data?.https?.state === 'requesting' ? 5_000 : false),
  })
  const { data: dns, isError: dnsFailed } = useQuery({
    ...domainDNSQuery,
    // Ask again until the admin has made the record.
    refetchInterval: (query) => (['missing', 'other', 'error'].includes(query.state.data?.state ?? '') ? 10_000 : false),
  })

  if (error && !settings) {
    return (
      <SettingsColumn>
        <LoadError what="the settings" error={error} onRetry={() => void refetch()} retrying={isFetching} />
      </SettingsColumn>
    )
  }
  if (!settings) return <GeneralSkeleton admin={admin} />

  const disabled = !admin
  const record = serverRecord(dns?.server_addresses ?? [])
  return (
    <SettingsColumn>
      {!admin && <ReadOnlyNote />}
      <SettingsSection title="Instance">
        <SettingRows>
          <TextSetting
            label="Name"
            description="Shown in the sidebar."
            value={settings.instance_name}
            toPatch={(instance_name) => ({ instance_name })}
            maxLength={settingLimits.instance_name.maxLength}
            disabled={disabled}
          />
          <TextSetting
            label="Dashboard URL"
            description="Used for links in notifications and invites."
            type="url"
            value={settings.public_url}
            toPatch={(public_url) => ({ public_url })}
            placeholder={window.location.origin}
            disabled={disabled}
          />
        </SettingRows>
      </SettingsSection>

      <SettingsSection title="HTTPS">
        <SettingRows>
          <TextSetting
            label="Domain"
            description={
              record
                ? `Point the ${record.type} record of the domain to ${record.value}. runplane serves HTTPS on ports 443 and 80.`
                : 'runplane serves HTTPS on ports 443 and 80.'
            }
            value={settings.domain}
            toPatch={(domain) => ({ domain })}
            placeholder="ci.example.com"
            disabled={disabled}
          />
          <TextSetting
            label="Contact email"
            description="For the certificate authority. Optional."
            type="email"
            value={settings.acme_email}
            toPatch={(acme_email) => ({ acme_email })}
            disabled={disabled}
          />
          {settings.domain && <DNSRow domain={settings.domain} dns={dns} failed={dnsFailed} />}
          {settings.domain && <CertificateRow domain={settings.domain} certificate={settings.https} />}
        </SettingRows>
      </SettingsSection>

      <SettingsSection title="Data retention">
        <SettingRows>
          <NumberSetting
            label="Jobs"
            description="Older jobs are deleted with their steps and metrics."
            value={settings.job_retention_days}
            toPatch={(job_retention_days) => ({ job_retention_days })}
            unit="days"
            limit={settingLimits.job_retention_days}
            disabled={disabled}
          />
          <NumberSetting
            label="Logs"
            description="After this time a job keeps its steps but not its log."
            value={settings.log_retention_days}
            toPatch={(log_retention_days) => ({ log_retention_days })}
            unit="days"
            limit={settingLimits.log_retention_days}
            disabled={disabled}
          />
        </SettingRows>
      </SettingsSection>

      {admin && <Updates autoUpdate={settings.auto_update} />}
      {admin && <Backups settings={settings} />}
    </SettingsColumn>
  )
}

const settingLimits = limits.SettingsPatch

type DNSRowProps = {
  domain: string
  dns: Schemas['DomainDNS'] | undefined
  /** The request for the check failed. */
  failed: boolean
}

/**
 * Whether the DNS record of the domain points to this server. Just after a
 * save, the last check is for the old domain, so the row shows "Checking".
 */
function DNSRow({ domain, dns, failed }: DNSRowProps) {
  const status = dns?.domain === domain ? dnsStatus(dns) : undefined
  return (
    <SettingRow group label="DNS record" description={status?.help || `runplane looks up ${domain}.`}>
      <p role="status" className="flex max-w-[280px] items-center gap-xs text-s text-neutral-10">
        {!status && failed ? (
          <>
            <X size={14} className="shrink-0" aria-hidden />
            Could not check the record
          </>
        ) : !status ? (
          <>
            <Spinner size={14} />
            Checking
          </>
        ) : (
          <>
            {status.tone === 'ok' && <Check size={14} className="shrink-0" aria-hidden />}
            {status.tone === 'error' && <X size={14} className="shrink-0" aria-hidden />}
            <span className="min-w-0 break-words">{status.text}</span>
          </>
        )}
      </p>
    </SettingRow>
  )
}

type CertificateRowProps = {
  domain: string
  certificate: Schemas['HTTPSStatus'] | undefined
}

/**
 * The certificate of the domain. A runplane process that cannot report it sends
 * no `https`, which hides the row. runplane requests the certificate on the
 * first HTTPS visit to the domain, not when the domain is saved.
 */
function CertificateRow({ domain, certificate }: CertificateRowProps) {
  if (!certificate || certificate.state === 'none') return null
  return (
    <SettingRow group label="Certificate" description="Let’s Encrypt · renews 30 days before it expires">
      <p role="status" className="flex max-w-[280px] items-center gap-xs text-s text-neutral-10">
        {certificate.state === 'requesting' ? (
          <>
            <Spinner size={14} />
            <span className="min-w-0 break-words">
              Waiting for the first visit to{' '}
              <a href={`https://${domain}`} target="_blank" rel="noreferrer" className="underline underline-offset-2">
                {domain}
                <span className="sr-only"> (opens in a new tab)</span>
              </a>
            </span>
          </>
        ) : certificate.state === 'error' ? (
          <>
            <X size={14} className="shrink-0" aria-hidden />
            {certificate.error || 'The certificate could not be issued.'}
          </>
        ) : (
          <>
            <Check size={14} className="shrink-0" aria-hidden />
            {certificate.not_after ? `Valid until ${formatDate(certificate.not_after)}` : 'Valid'}
          </>
        )}
      </p>
    </SettingRow>
  )
}

function GeneralSkeleton({ admin }: { admin: boolean }) {
  return (
    <SettingsColumn>
      <SettingsSection title="Instance">
        <SettingRowsSkeleton />
      </SettingsSection>
      <SettingsSection title="HTTPS">
        <SettingRowsSkeleton />
      </SettingsSection>
      <SettingsSection title="Data retention">
        <SettingRowsSkeleton control="number" />
      </SettingsSection>
      {admin && (
        <SettingsSection title="Updates">
          <SettingRowsSkeleton control="switch" />
        </SettingsSection>
      )}
    </SettingsColumn>
  )
}
