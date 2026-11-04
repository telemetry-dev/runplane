import type { Schemas } from '~/api/client'

type HostCheck = Schemas['HostCheck']
type Check = Schemas['Check']

/**
 * Whether `code` has the shape of a setup code, as "7KQM-R4TX-W9HB". The
 * alphabet leaves out I, O, 0, and 1; the dashes are optional.
 */
export function isSetupCode(code: string): boolean {
  return /^[A-HJ-NP-Z2-9]{4}-?[A-HJ-NP-Z2-9]{4}-?[A-HJ-NP-Z2-9]{4}$/i.test(code)
}

/**
 * The checks to list: failed ones first, then warnings, then passed ones, each
 * group in API order. The summary above the list shows the mode, so the mode
 * check is left out.
 */
export function listedChecks(checks: Check[]): Check[] {
  return checks.filter((check) => check.id !== 'mode').toSorted((a, b) => statusOrder[a.status] - statusOrder[b.status])
}

const statusOrder: Record<Check['status'], number> = { fail: 0, warn: 1, ok: 2 }

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
