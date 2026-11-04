// The text of the list settings: DNS servers in one input, the egress allowlist in a textarea.

/** The items of a list setting: one on each line, or separated by commas or spaces. */
export function listItems(text: string, lines: boolean): string[] {
  return text
    .split(lines ? '\n' : /[\s,]+/)
    .map((item) => item.trim())
    .filter(Boolean)
}

/** The 1-based number of the line that holds item `index` of `listItems(text, true)`. */
export function lineNumber(text: string, index: number): number {
  let seen = -1
  const lines = text.split('\n')
  for (const [number, line] of lines.entries()) {
    if (line.trim() && ++seen === index) return number + 1
  }
  return index + 1
}
