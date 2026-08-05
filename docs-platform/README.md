# ObjectFS Documentation Site

A [VitePress](https://vitepress.dev) site. Markdown in, static HTML out — that is the whole of it.

## Working on it

```bash
npm ci          # against the committed lockfile, not a fresh range resolution
npm run dev     # http://localhost:5173, with hot reload
npm run build   # static output in .vitepress/dist
npm run preview # serve what build produced
```

`npm run build` is what the `docs-site` job in `.github/workflows/ci.yml` runs on every pull request.
That job is the reason this tree builds at all: nothing invoked the builder for the whole of its
history, and three independent breakages had accumulated behind that silence — each visible only once
the previous was fixed (#214). A step no job runs is not a gate.

## Layout

```
docs-platform/
├── .vitepress/
│   ├── config.js       # nav, sidebar, search
│   └── theme/          # the default theme plus custom.css
├── guide/              # narrative documentation
├── playground/         # SDK and CLI examples
└── index.md            # landing page
```

Note the absences. There is no `src/`, no `client/`, no `api/`, no `tutorials/` and no `sdks/` — and
this file used to draw five of those in this diagram (#336). The deeper reference documentation lives
in `docs/` at the repository root, published by MkDocs.

## What this directory is not

It is not an application. It described itself as an "interactive documentation platform" and carried
the machinery to look like one: a 549-line Express server in `src/api-server.js`, an `ApiPlayground`
component, a `CodeRunner` component, a `Dockerfile`, a seven-service `docker-compose.yml`, and
thirteen runtime dependencies. All of it is gone (#336), because nothing built any of it and none of
it worked:

- **Nothing ran the server.** No CI job, no deployment, no `make` target. `npm start` pointed at
  `src/server.js`, which has never existed in this repository — so the obvious command to type failed
  immediately, and had for the whole life of the file. `npm run dev`, `lint`, `format` and `build`
  named absent paths too.
- **Both components called that server.** `CodeRunner`'s "Run" button POSTed to
  `/api/code-runner/execute`, `ApiPlayground`'s "Send Request" to `/api-playground/...`. With no
  server they were inert on every page they ever appeared on, while looking exactly like controls that
  work. That is a worse failure than a build error, and the reader is who it happens to.
- **Six of the seven endpoints the playground offered do not exist in ObjectFS.** A running mount
  serves `/health` and `/metrics`, on the addresses `monitoring.health_checks.addr` and
  `monitoring.metrics.addr` configure, and nothing else. `curl` reaches those from a terminal.
- **The `Dockerfile` could not build.** It `COPY`ed `api/`, `tutorials/`, `sdks/` and `public/`, none
  of which exist here, so it failed on the first missing directory.
- **`docker-compose.yml` was not a deployment.** It bind-mounted `./nginx.conf`, `./ssl`,
  `./prometheus.yml` and `./grafana/` — four paths absent from this directory — and pulled
  `objectfs/objectfs:latest`, an image this project does not publish; releases go to
  `ghcr.io/scttfrdmn/objectfs`. Real deployment manifests are tracked in #146.

The dependency list was the measure of the gap: `express`, `cors`, `helmet`, `compression`, `morgan`,
`swagger-ui-express`, `swagger-jsdoc`, `socket.io`, `dockerode`, `axios`, `dotenv`, `prismjs` and
`yaml`. Every one was there for the server, and two of them (`prismjs`, `axios`) had no reference
anywhere in the tree at all. Removing them took `npm ci` from 689 packages to 130, and `npm audit`
reports zero vulnerabilities. The `uuid` override went with them: it existed for a bounds-check
advisory reachable only through `dockerode` → `docker-modem`.

Code blocks are plain fenced blocks now. VitePress puts a copy button on each one, which is a thing a
reader can actually use.

## Adding a page

1. Write the markdown.
2. Add it to the nav or sidebar in `.vitepress/config.js`.
3. `npm run build` — a link to a page that does not exist fails the build, deliberately.

Relative links that leave this directory are dead links to the builder even though they resolve on
GitHub, so anything outside `docs-platform/` has to be an absolute URL.
`internal/config/docs_links_test.go` checks every link in the repository and treats root-absolute
paths here as VitePress routes: `/guide/installation` resolves to `guide/installation.md`.

## License

Apache License 2.0 — see
[LICENSE](https://github.com/scttfrdmn/objectfs/blob/main/LICENSE) for details.

The link is absolute rather than `../LICENSE` for the reason given above: this directory is a
VitePress site root, and a relative path that leaves it fails the build.
