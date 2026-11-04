// Copies the static SPA (dist/client) into ../internal/webui/dist for go:embed.
// Keeps internal/webui/dist/.keep so the folder stays in git.
import { cp, mkdir, readdir, rm } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const source = fileURLToPath(new URL('../dist/client/', import.meta.url))
const target = fileURLToPath(new URL('../../internal/webui/dist/', import.meta.url))

if (!existsSync(`${source}index.html`)) {
  throw new Error(`no SPA build in ${source}: run pnpm build first`)
}

await mkdir(target, { recursive: true })
for (const entry of await readdir(target)) {
  if (entry !== '.keep') await rm(`${target}${entry}`, { recursive: true, force: true })
}
await cp(source, target, { recursive: true })
console.log(`copied ${source} to ${target}`)
