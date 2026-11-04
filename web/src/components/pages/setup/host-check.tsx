import { RefreshCw } from 'lucide-react'
import { useEffect, useState } from 'react'
import { $api, errorMessage, isApiError, type Schemas } from '~/api/client'
import { KeyValue, KeyValueList } from '~/components/app/key-value'
import { SectionButton } from '~/components/app/section-header'
import { CheckIcon } from '~/components/app/status-icon'
import { FixCommand } from '~/components/pages/servers/fix-command'
import { Spinner } from '~/components/ui/spinner'
import { cx } from '~/lib/cx'
import { formatBytes, formatMegabytes } from '~/lib/format'
import { isSetupCode, listedChecks, modeReason, modeTitles } from './checks'

type Check = Schemas['Check']

/**
 * The host check for the setup code being typed. It starts once the code has
 * the shape of one and typing pauses, and runs again only on "Check again":
 * it takes about 3 seconds, because it asks an NTP server for the time.
 *
 * `codeError` is the message for the code field when the check rejected the
 * code that the field holds now.
 */
export function useHostCheck(code: string) {
  // The server ignores spaces and case, as in "7kqm r4tx w9hb".
  const typed = code.replace(/\s/g, '').toUpperCase()
  const checked = useDebouncedValue(typed, 300)
  const ready = isSetupCode(checked)
  const query = $api.useQuery(
    'get',
    '/api/setup/host',
    { params: { query: { code: checked } } },
    { enabled: ready, retry: false, staleTime: Infinity, refetchOnWindowFocus: false },
  )
  const codeError = typed === checked && isApiError(query.error, 422) ? errorMessage(query.error) : null
  return { query, ready, codeError }
}

type HostCheckProps = Pick<ReturnType<typeof useHostCheck>, 'query' | 'ready'>

/**
 * "This server": the mode, size, and doctor checks of the server that runs the
 * setup. A wrong setup code (422) shows under the code field, and a finished
 * setup (409) sends the user to log in, so the box asks for the code then.
 */
export function HostCheck({ query, ready }: HostCheckProps) {
  const codeAccepted = ready && !isApiError(query.error, 422) && !isApiError(query.error, 409)
  return (
    <section aria-labelledby="host-check-title" className="flex flex-col gap-xs">
      <div className="flex items-center justify-between gap-m">
        <h2 id="host-check-title" className="flex min-w-0 items-baseline gap-xs text-s font-medium text-neutral-10">
          This server
          {query.data?.hostname && <span className="truncate font-regular text-neutral-7">{query.data.hostname}</span>}
        </h2>
        <SectionButton
          disabled={!codeAccepted || query.isFetching}
          focusableWhenDisabled={query.isFetching}
          onClick={() => void query.refetch()}
        >
          {query.isFetching ? <Spinner size={14} /> : <RefreshCw size={14} aria-hidden />}
          Check again
        </SectionButton>
      </div>
      {/* The padding gives the edge its own pixel, as a border would. */}
      <div className="flex flex-col overflow-clip rounded-m bg-neutral-1 p-px shadow-[inset_0_0_0_1px_var(--neutral-4)]">
        {!codeAccepted ? (
          <p className="px-m py-s text-s text-neutral-7">Enter the setup code to check this server.</p>
        ) : query.isPending ? (
          <p role="status" className="flex items-center gap-s px-m py-s text-s font-medium text-neutral-10">
            <Spinner size={16} />
            Checking this server
          </p>
        ) : query.isError ? (
          <p className="px-m py-s text-s text-neutral-7">{errorMessage(query.error)}</p>
        ) : (
          <HostReport host={query.data} />
        )}
      </div>
    </section>
  )
}

const divider = 'not-last:border-b not-last:border-neutral-4'

function HostReport({ host }: { host: Schemas['HostCheck'] }) {
  const checks = listedChecks(host.checks)
  return (
    <>
      <div className={cx('flex flex-col gap-l px-m py-l', divider)}>
        <div className="flex flex-col gap-xxs">
          <h3 className="text-m font-heavy text-neutral-10">{modeTitles[host.mode]}</h3>
          <p className="text-s text-neutral-8">{modeReason(host)}</p>
        </div>
        <KeyValueList className="gap-x-xl">
          <KeyValue label="CPU">
            {host.vcpu} vCPU · {host.arch}
          </KeyValue>
          <KeyValue label="Memory">{formatMegabytes(host.memory_mb)}</KeyValue>
          <KeyValue label="Disk">{disk(host)}</KeyValue>
          <KeyValue label="System">{host.os}</KeyValue>
        </KeyValueList>
      </div>
      {checks.length > 0 && (
        <ul aria-label="Checks">
          {checks.map((check) => (
            <CheckRow key={check.id} check={check} />
          ))}
        </ul>
      )}
    </>
  )
}

function CheckRow({ check }: { check: Check }) {
  const ok = check.status === 'ok'
  return (
    <li className={cx('flex flex-col gap-xs px-m', ok ? 'py-xs' : 'py-s', divider)}>
      <div className="flex items-start gap-s text-s">
        {/* Centered on the first line of the row. */}
        <span className="flex h-(--line-s) w-[14px] shrink-0 items-center">
          <CheckIcon status={check.status} size={14} />
        </span>
        {/* Wide enough for "Sandbox internet access" on one line at both weights. */}
        <span className={cx('w-[160px] shrink-0', ok ? 'text-neutral-8' : 'font-medium text-neutral-10')}>{check.name}</span>
        <span className="flex min-w-0 flex-1 flex-col gap-xxs break-words">
          <span className={ok ? 'text-neutral-7' : 'text-neutral-10'}>{check.detail}</span>
          {!ok && check.fix && <span className="text-neutral-8">{check.fix}</span>}
        </span>
      </div>
      {/* The left margin lines the command up with the name: the 14px icon plus the 9px (space-s) gap. */}
      {!ok && check.command && <FixCommand command={check.command} className="ml-[23px]" />}
    </li>
  )
}

/** "1.9 TB NVMe · 1.7 TB free" */
function disk(host: Schemas['HostCheck']): string {
  const size = [formatBytes(host.disk_bytes), host.disk_kind].filter(Boolean).join(' ')
  return host.free_disk_bytes > 0 ? `${size} · ${formatBytes(host.free_disk_bytes)} free` : size
}

function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(timer)
  }, [value, delayMs])
  return debounced
}
