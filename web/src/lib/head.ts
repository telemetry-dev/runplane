/** Route `head` with the document title "<page> · runplane". Use as `head: () => pageHead('Jobs')`. */
export function pageHead(page: string) {
  return { meta: [{ title: `${page} · runplane` }] }
}
