/** The dash of an empty value. Screen readers read it as "None". */
export function None() {
  return (
    <span className="text-neutral-6">
      <span aria-hidden>—</span>
      <span className="sr-only">None</span>
    </span>
  )
}
