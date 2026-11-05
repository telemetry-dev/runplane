import type { ReactNode } from 'react'
import { KeyValue, KeyValueList } from '~/components/app/key-value'
import { None } from '~/components/app/none'
import { Stat } from '~/components/app/stat'
import { serverStatusNames } from '~/components/app/status-dot'
import { UsageBar } from '~/components/app/usage-bar'
import { agentUpdateNote, currentUsage, serverLocation } from '~/components/pages/servers/server-info'
import { Tooltip } from '~/components/ui/tooltip'
import { formatPercent } from '~/lib/format'
import { useNow } from '~/lib/use-now'
import { diskDetail, memoryDetail, sandboxMode, statusDetail, type Server } from './server-format'

/** The facts row under the title: status, address, provider, system, mode, KVM, and version. */
export function ServerFacts({ server }: { server: Server }) {
  const now = useNow(5000)
  const detail = statusDetail(server, now)
  const mode = sandboxMode(server)
  const reported = server.arch !== ''
  const update = agentUpdateNote(server)
  // The API puts the reason of a failed update in the status detail.
  const failure = server.update_state === 'failed' ? server.status_detail : ''
  return (
    // The facts sit closer to the title than the page gap, as in Job detail.
    <KeyValueList className="-mt-m">
      <KeyValue label="Status">
        {serverStatusNames[server.status]}
        {detail && <span className="text-neutral-7"> · {detail}</span>}
      </KeyValue>
      <KeyValue label="Address">{server.address || <None />}</KeyValue>
      <KeyValue label="Provider">{serverLocation(server) || <None />}</KeyValue>
      <KeyValue label="OS">{server.os || <None />}</KeyValue>
      <KeyValue label="Kernel">{server.kernel || <None />}</KeyValue>
      <KeyValue label="Mode">
        {mode ? (
          <>
            {mode.name}
            {mode.detail && <span className="text-neutral-7"> · {mode.detail}</span>}
          </>
        ) : (
          <None />
        )}
      </KeyValue>
      <KeyValue label="KVM">{reported ? (server.kvm ? 'Yes' : 'No') : <None />}</KeyValue>
      <KeyValue label="runplane">
        {server.version.replace(/^v/, '') || <None />}
        {update && <span className="text-neutral-7"> · {failure ? <WhyHint why={failure}>{update}</WhyHint> : update}</span>}
      </KeyValue>
    </KeyValueList>
  )
}

/** CPU, memory, and disk of the host. */
export function ServerUsage({ server }: { server: Server }) {
  const usage = currentUsage(server)
  const vcpu = server.vcpu ? `${server.vcpu_in_use} of ${server.vcpu} vCPU allocated to runners` : ''
  return (
    <div className="@container">
      <div className="grid gap-xl @2xl:grid-cols-3 @2xl:gap-xxl">
        <Usage label="CPU" value={usage.cpu} detail={vcpu} />
        <Usage label="Memory" value={usage.memory} detail={server.memory_mb ? memoryDetail(server) : ''} />
        <Usage label="Disk" value={usage.disk} detail={diskDetail(server, usage.disk)} />
      </div>
    </div>
  )
}

function Usage({ label, value, detail }: { label: string; value: number | null; detail: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-xs">
      <Stat label={label} value={value === null ? null : formatPercent(value)} />
      {value !== null && <UsageBar value={value} label={label} className="w-full" />}
      {detail && <p className="truncate text-xs text-neutral-7">{detail}</p>}
    </div>
  )
}

/** Text with a dotted underline that shows `why` in a tooltip. It takes keyboard focus, and screen readers read `why` with it. */
function WhyHint({ why, children }: { why: string; children: ReactNode }) {
  return (
    <Tooltip content={why}>
      <span tabIndex={0} className="cursor-default rounded-xs underline decoration-dotted underline-offset-2">
        {children}
        <span className="sr-only">: {why}</span>
      </span>
    </Tooltip>
  )
}
