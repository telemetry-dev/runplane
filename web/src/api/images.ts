import type { QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'
import { poolsQuery } from './pools'

type Image = Schemas['Image']

export const imagesQuery = $api.queryOptions('get', '/api/images')

/** Puts an image from a mutation response into the cached list, as a new row if it is new. */
export function putImage(client: QueryClient, image: Image) {
  client.setQueryData<Image[]>(imagesQuery.queryKey, (list) => [...(list ?? []).filter((item) => item.id !== image.id), image])
}

/** Removes an image from the cached list, and refetches the pools, which change to the default image. */
export function dropImage(client: QueryClient, id: string) {
  client.setQueryData<Image[]>(imagesQuery.queryKey, (list) => list?.filter((item) => item.id !== id))
  return client.invalidateQueries({ queryKey: poolsQuery.queryKey })
}
