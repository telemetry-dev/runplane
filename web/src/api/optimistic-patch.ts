import { useMutation, useQueryClient, type QueryKey } from '@tanstack/react-query'

type OptimisticPatch<TData, TVariables, TResult> = {
  /** The cached query that shows the change. */
  queryKey: QueryKey
  mutationFn: (variables: TVariables) => Promise<TResult>
  /** The cached data with the change. */
  apply: (data: TData, variables: TVariables) => TData
  /** The variables of the change that puts back the values that `variables` changes, from the data before it. */
  undo: (variables: TVariables, before: TData) => TVariables
  onSuccess?: (result: TResult, variables: TVariables) => unknown
  onError?: (error: Error, variables: TVariables) => unknown
}

/**
 * A change that the cached query shows at once, as a switch that moves before
 * the API answers. If the API refuses it, only the values that it changed go
 * back: other controls can save in the same time.
 */
export function useOptimisticPatch<TData, TVariables, TResult>({
  queryKey,
  mutationFn,
  apply,
  undo,
  onSuccess,
  onError,
}: OptimisticPatch<TData, TVariables, TResult>) {
  const client = useQueryClient()
  return useMutation({
    mutationFn,
    onMutate: async (variables) => {
      await client.cancelQueries({ queryKey })
      const before = client.getQueryData<TData>(queryKey)
      client.setQueryData<TData>(queryKey, (data) => data && apply(data, variables))
      return { before }
    },
    onError: (error, variables, context) => {
      const before = context?.before
      if (before !== undefined) client.setQueryData<TData>(queryKey, (data) => data && apply(data, undo(variables, before)))
      return onError?.(error, variables)
    },
    onSuccess,
  })
}
