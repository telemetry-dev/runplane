/** The search params that the GitHub callbacks send the browser back with. */
export type CallbackSearch = {
  /** GitHub did not finish the connection. */
  error?: string
  /** GitHub sent the install request to an organization owner, who must approve it. */
  requested?: true
}

export function callbackSearch(search: Record<string, unknown>): CallbackSearch {
  const { error, requested } = search
  return {
    error: typeof error === 'string' ? error : undefined,
    // The router parses ?requested=1 as the number 1.
    requested: requested === 1 || requested === '1' || requested === true ? true : undefined,
  }
}
