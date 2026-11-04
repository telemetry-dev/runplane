import { BranchIcon } from '~/components/ui/icons'

type BranchRefProps = {
  branch: string
  /** The commit; the first 7 characters show under the branch. */
  sha?: string
}

/** Branch name after a git-branch icon, with the short commit below it in mono, as in the Jobs table. */
export function BranchRef({ branch, sha }: BranchRefProps) {
  return (
    <div className="flex min-w-0 flex-col">
      <div className="flex min-w-0 items-center gap-xs">
        <BranchIcon size={14} className="text-neutral-7" aria-hidden />
        <span className="truncate text-s text-neutral-8">{branch}</span>
      </div>
      {sha && (
        // The designs give the commit line 16px, which makes a job row 53px tall.
        <span className="pl-[calc(14px+var(--space-xs))] font-code text-xs leading-[16px] text-neutral-7" title={sha}>
          {sha.slice(0, 7)}
        </span>
      )}
    </div>
  )
}
