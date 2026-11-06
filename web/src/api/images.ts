import type { QueryClient } from '@tanstack/react-query'
import { $api, type Schemas } from './client'

type Image = Schemas['Image']

export const imagesQuery = $api.queryOptions('get', '/api/images')

/** Puts an image from a mutation response into the cached list, as a new row if it is new. */
export function putImage(client: QueryClient, image: Image) {
  client.setQueryData<Image[]>(imagesQuery.queryKey, (list) => [...(list ?? []).filter((item) => item.id !== image.id), image])
}

export function dropImage(client: QueryClient, id: string) {
  client.setQueryData<Image[]>(imagesQuery.queryKey, (list) => list?.filter((item) => item.id !== id))
}
