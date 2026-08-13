# Regenerating the dashboard timeline bundle

`internal/server/dashboard/assets/timeline.js` is a ts0-compiled bundle of the
TypeScript adapter in `internal/server/dashboard/ts/`. It is **committed**, and
that is deliberate: `go:embed` needs it present on a fresh clone, so a normal
`go build` never runs Node.

Never hand-edit the bundle — it carries a DO-NOT-EDIT banner. Edit `ts/`, then:

```sh
go generate ./internal/server/dashboard/
```

and commit the regenerated bundle alongside the source.

## How it runs

`generate.go` carries `//go:generate go run ./gen`, and the generator is
`gen/main.go`.

It lives in its own directory on purpose. The obvious alternative — one
`gen.go` beside `dashboard.go` carrying `//go:build ignore` — puts a
`package main` file in a `package dashboard` directory, which fails any load
that disregards build tags. go-toolchain vets under `ignore` among other tags,
and the tag cannot save the file there:

```
found packages dashboard (dashboard.go) and main (gen.go) in internal/server/dashboard
```

(The standard library has the same latent conflict — `strconv/makeisprint.go`,
`sort/gen_sort_variants.go` — which is why `-tags ignore` cannot build stdlib
either.) A generator alone in its own package has no such conflict, needs no
build tag, and is never imported so it never ships.

What it does is the recipe from [ts0's own
README](https://github.com/wow-look-at-my/ts0#prebuilt-ts0cjs-buildhost) for
build wiring, in Go so the directive is portable:

```sh
curl -fL "https://dl.pazer.build/ts0?v=N&os=linux&arch=amd64" -o ts0.cjs
node ts0.cjs build
```

Node 22+ is the only requirement. No npm, npx, node_modules or git — ts0 fetches
its one native piece (esbuild) into its own cache on first run.

Two details worth knowing before editing `gen/main.go`:

- **`ts0.cjs` is platform-neutral.** Buildhost addresses artifacts by os/arch so
  the URL parameters are required, but every supported pair returns identical
  bytes. There is nothing to branch on; the URL is a constant.
- **The `?v=N` pin is what makes regeneration byte-reproducible.** `?branch=master`
  moves on every merge and would make the freshness gate flap. Bump `ts0Version`
  in `gen/main.go` and commit the resulting bundle change in the same commit.

`WHR_TS0_CJS` points the generator at a pre-fetched bundle for offline use.

## The freshness gate

CI does not add a job for this. `ci.yml`'s `test` job passes go-toolchain a
`generate:` approval hash, so go-toolchain runs the directive and then fails on
a dirty working tree — a committed bundle that is stale versus `ts/` turns the
build red instead of drifting silently. `actions/setup-node@v4` is there to give
that step a Node.

The approval hash re-keys only when the directive LINE in `generate.go` is
edited or moved; a bare `go-toolchain` run prints the new one.

## Why not the buildhost download action

`wow-look-at-my/buildhost/.github/actions/buildhost-download` fetches exactly
this kind of artifact, and it is the right tool for a workflow STEP. It is not
used here because the fetch has to work on a laptop too: `go generate` is the
supported entry point, so the download cannot live in a workflow. Wiring the
action in as well would leave the version pinned in two places, free to drift.
