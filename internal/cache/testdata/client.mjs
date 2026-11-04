// Saves or restores the "data" folder with @actions/cache, like actions/cache does.
// Usage: node client.mjs <path to @actions/cache lib/cache.js> save <key>
//        node client.mjs <path to @actions/cache lib/cache.js> restore <key> [restore keys...]
// The last stdout line is the JSON result.
const [modulePath, op, key, ...restoreKeys] = process.argv.slice(2)
const cache = await import(modulePath)

let result
if (op === 'save') {
  result = {id: await cache.saveCache(['data'], key)}
} else {
  result = {matched: (await cache.restoreCache(['data'], key, restoreKeys)) ?? null}
}
console.log('RESULT ' + JSON.stringify(result))
