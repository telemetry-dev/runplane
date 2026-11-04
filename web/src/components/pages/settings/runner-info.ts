import type { Schemas } from '~/api/client'
import type { JobStatus } from '~/components/app/status-icon'

type Build = Schemas['ImageBuild']
type BuildState = Build['status']
type RunnerVersion = Schemas['RunnerVersion']

/** Labels of the build states, in the order of the "Servers" summary. */
const buildLabels: [BuildState, string][] = [
  ['ready', 'ready'],
  ['pulling', 'pulling'],
  ['building', 'building'],
  ['converting', 'converting'],
  ['pending', 'waiting'],
  ['failed', 'failed'],
]

/** The "Servers" cell of an image: "5 ready · 1 pulling". */
export function buildSummary(builds: Build[]): string {
  return buildLabels
    .map(([state, label]) => [builds.filter((build) => build.status === state).length, label] as const)
    .filter(([count]) => count > 0)
    .map(([count, label]) => `${count} ${label}`)
    .join(' · ')
}

/** "Ready", "Waiting", …, for one server's row of an image. */
export function buildLabel(state: BuildState): string {
  const label = buildLabels.find(([key]) => key === state)![1]
  return label[0]!.toUpperCase() + label.slice(1)
}

export function buildStatus(state: BuildState): JobStatus {
  if (state === 'ready') return 'succeeded'
  if (state === 'pending') return 'queued'
  if (state === 'failed') return 'failed'
  return 'running'
}

/** The "Used by" cell: "5 labels", "1 label", or "No labels". */
export function usedBy(poolCount: number): string {
  if (poolCount === 0) return 'No labels'
  return `${poolCount} ${poolCount === 1 ? 'label' : 'labels'}`
}

export type VersionGroup = {
  version: string
  /** "x64, arm64". */
  architectures: string
  status: JobStatus
  /** The errors of the failed architectures. */
  error: string
  released_at: string | null
  end_date: string | null
  ends_soon: boolean
  /** "In use", or "In use · x64" when only some architectures use it: new sandboxes there get this version. */
  inUse: string | null
}

/**
 * The runner bundles of GET /api/runner-versions, one entry per version, in the
 * order of the API (newest first). `current` is the version of each arch that
 * new sandboxes get.
 */
export function groupVersions(items: RunnerVersion[], current: Schemas['RunnerCurrent']): VersionGroup[] {
  const usedArches = archOrder.filter((arch) => current[arch])
  const versions = new Map<string, RunnerVersion[]>()
  for (const item of items) versions.set(item.version, [...(versions.get(item.version) ?? []), item])
  return [...versions].map(([version, bundles]): VersionGroup => {
    const failed = bundles.filter((bundle) => bundle.status === 'failed')
    const arches = bundles.map((bundle) => bundle.arch).toSorted((a, b) => archOrder.indexOf(a) - archOrder.indexOf(b))
    return {
      version,
      architectures: arches.join(', '),
      status: failed.length > 0 ? 'failed' : bundles.some((bundle) => bundle.status === 'pending') ? 'running' : 'succeeded',
      error: failed.map((bundle) => `${bundle.arch}: ${bundle.error || 'The download failed.'}`).join(' '),
      released_at: bundles.find((bundle) => bundle.released_at)?.released_at ?? null,
      end_date: bundles.find((bundle) => bundle.end_date)?.end_date ?? null,
      ends_soon: bundles.some((bundle) => bundle.ends_soon),
      inUse: inUse(version, usedArches, current),
    }
  })
}

function inUse(version: string, arches: Arch[], current: Schemas['RunnerCurrent']): string | null {
  const using = arches.filter((arch) => current[arch] === version)
  if (using.length === 0) return null
  return using.length === arches.length ? 'In use' : `In use · ${using.join(', ')}`
}

type Arch = keyof Schemas['RunnerCurrent']

const archOrder: Arch[] = ['x64', 'arm64']
