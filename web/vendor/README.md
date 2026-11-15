# Vendored libraries of the web app

Downloaded once from the npm registry and committed; nothing is fetched at
run time. The hub serves this directory under `/vendor/` with a cache
lifetime of a year, so every file name carries its version: a new version
goes into a new directory, and the app's imports change with it.

| Library | Version | Source | Tarball SHA-256 | Licence |
|---|---|---|---|---|
| Preact (core and hooks) | 10.29.8 | https://registry.npmjs.org/preact/-/preact-10.29.8.tgz | `b18cb0a457f3d43c7bb30391a74ade7d13e03bc6e77915e061c70c0fe1123299` | MIT (`preact-10.29.8/LICENSE`) |
| htm | 3.1.1 | https://registry.npmjs.org/htm/-/htm-3.1.1.tgz | `2425b9bee11409177bcabc7f32e319926fc6690c1701c0b257c88bdff2d5ba90` | Apache-2.0 (`htm-3.1.1/LICENSE`) |
| xterm.js (`@xterm/xterm`) | 6.0.0 | https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz | `908e66e04af6c8dc6b00dd3b54de088e2e81e5ed866284fd6c2fb3c2d1c7a3f6` | MIT (`xterm-6.0.0/LICENSE`) |

npm integrity values (SHA-512) of the same tarballs:

- preact 10.29.8: `sha512-ej2aVZ+vZ8WO7tvlQWRM9N63A0KzF9q4mWJfDUHgYaIofWY9hu74QdnQrjoPMmZi2/nZ5gN0bJCQF49xQqx09Q==`
- htm 3.1.1: `sha512-983Vyg8NwUE7JkZ6NmOqpCZ+sh1bKv2iYTlUkzlWmA5JD2acKoxd4KVxbMmxX/85mtfdnDmTFoNKcg5DGAvxNQ==`
- @xterm/xterm 6.0.0: `sha512-TQwDdQGtwwDt+2cgKDLn0IRaSxYu1tSUjgKarSDkUM0ZNiSRXFpjxEsvc/Zgc5kq5omJ+V0a8/kIM2WD3sMOYg==`

## Files

| File | Taken from (in the tarball) | SHA-256 |
|---|---|---|
| `preact-10.29.8/preact.module.js` | `package/dist/preact.module.js` | `c30e721ebfdc6e2ad4c18c14d2dfb82667829c8aec27de1207774e3fc16858a8` |
| `preact-10.29.8/hooks.module.js` | `package/hooks/dist/hooks.module.js`, edited (below) | `f3555e7a8dba84175c94f17148ba6670e4b3716888fe06b3330a485637296a38` |
| `htm-3.1.1/htm.module.js` | `package/dist/htm.module.js` | `ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4` |
| `xterm-6.0.0/xterm.mjs` | `package/lib/xterm.mjs` | `b336ec65a086c056d4804b3d4c2347da5663d3f23c3f25be866467bd8857ad59` |
| `xterm-6.0.0/xterm.css` | `package/css/xterm.css` | `854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6` |

**One edit.** The hooks module imports the bare specifier `"preact"`, which a
browser can resolve only through an import map, and an import map is an
inline script that the hub's content security policy forbids. The only change
is that import: `from"preact"` became `from"./preact.module.js"`. The
unedited file has SHA-256
`a6ee626f2d01570592dd569a792e3f050154aa02890eead8c223fa3ed5aa3d5a`.

The `sourceMappingURL` comments are left in place; the maps are not
vendored, so browser developer tools report them missing.

## xterm.js under the content security policy

xterm.js 6.0.0's DOM renderer creates `<style>` elements and sets `style`
attributes for 24-bit colours, both of which the policy (`default-src 'self'`,
no `'unsafe-inline'`) refuses. Instead of relaxing the policy, the app
installs two small shims before loading xterm.js (`web/app/cspshim.js`): a
`<style>` element created by script becomes a constructable stylesheet
adopted by the document, and `setAttribute("style", …)` becomes a CSSOM
assignment. Neither is restricted by the policy.

## Updating

Download the tarball of the new version, check its integrity against the
registry, copy the same files into a new directory named after the version,
repeat the edit of the hooks module, update the imports in `web/app/` and
`web/index.html`, update this file and remove the old directory.
