import type { Schemas } from '~/api/client'
import { limits } from '~/api/limits'

type HostCheck = Schemas['HostCheck']

const setupCode = new RegExp(limits.SetupRequest.code.pattern)

/** Whether `code` has the shape of a setup code, as "7KQM-R4TX-W9HB". */
export function isSetupCode(code: string): boolean {
  return setupCode.test(code)
}

export const modeTitles: Record<HostCheck['mode'], string> = {
  vm: 'VM mode',
  container: 'Container mode',
  dev: 'Dev mode',
}

/** Why this server runs jobs in its mode, and what that means for the user. */
export function modeReason({ mode, kvm }: Pick<HostCheck, 'mode' | 'kvm'>): string {
  if (mode === 'vm') return 'KVM is available, so each job runs in its own Firecracker microVM.'
  if (mode === 'dev') return 'Development machine: jobs run in privileged Docker containers.'
  if (kvm) return 'Jobs run in Sysbox containers: use them for trusted code only. This server has KVM, so VM mode is possible.'
  return 'This server has no KVM, so jobs run in Sysbox containers: use them for trusted code only.'
}
