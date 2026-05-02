# bort

`bort` is a migration orchestrator for self-hosted PaaS platforms. The first target is moving apps from Coolify to Dokploy with live traffic cutover, data sync, health checks, and rollback.

The project is intentionally starting as a small Go CLI. The core idea is to extract a source platform into a portable manifest, recreate the app on the target platform, sync state, validate privately, then flip traffic through a migration gateway.

## Current Status

This repository is at the local dry-run planning stage. It can scan local Docker resources, the Coolify API, or source-server Docker state; export a private migration bundle; validate topology; and produce read-only prepare, sync, cutover, rollback, and commit plans.

Implemented now:

- `bort scan --source docker` discovers local Docker containers, routes, mounts, volumes, and networks.
- `bort scan --source coolify` exports Coolify applications, services, databases, env vars, storages, compose config, git metadata, and domains through the Coolify API.
- `bort scan --source coolify-local` is intended to run on the source Coolify server and uses local Docker state as the migration-grade inventory path.
- `bort plan` reads a manifest and prints a first migration readiness summary.
- `bort export` writes a local migration bundle with compose, env, routes, storages, topology, and a report per app.
- `bort validate` checks exported bundles for compose validity, portability risks, missing routes, and secret handling.
- `bort prepare` reads an exported bundle and prints a dry-run target preparation plan, including Dokploy render specs, without creating resources.
- `bort sync` plans state sync work without copying data.
- `bort cutover` plans route cutover, health checks, observation, and rollback windows without changing routes.
- `bort rollback` plans route rollback to the source without changing routes.
- `bort commit` plans final target acceptance and source retirement without committing ownership or deleting source resources.
- Source/target/gateway/sync/state packages define the shape for the future live migration engine.

Not implemented yet:

- Dokploy resource creation.
- Migration gateway installation.
- Database replication adapters.
- Volume delta sync execution.
- Live route cutover, rollback, final commit, and source cleanup.

## Usage

Build the CLI:

```sh
go build -o bin/bort ./cmd/bort
```

Scan the local Docker host:

```sh
bin/bort scan --output manifest.json
```

Scan a Coolify instance:

```sh
export BORT_COOLIFY_URL=https://coolify.example.com
export BORT_COOLIFY_TOKEN=your-token-here
bin/bort scan --source coolify --output manifest.json
```

Coolify API tokens are only read from `BORT_COOLIFY_TOKEN`, not from a command-line flag, so they do not appear in the process table. Scan manifests are written with private file permissions.

Scan from the source Coolify server for a more complete migration inventory:

```sh
sudo bin/bort scan --source coolify-local --output manifest.json
```

The Coolify API scan is a safe preflight, but it is not the migration source of truth. Server-local scanning can see the actual Docker containers, images, networks, volumes, bind mounts, and labels that the API may omit.

`coolify-local` enriches Docker groups from Coolify labels when available, including resource names, resource type, project/environment, compose file paths, and whether a group looks like a migration candidate or Coolify platform support.

`bort plan` also infers topology from the manifest: Docker networks, internal dependencies such as Postgres or Redis services, first-class data stores with migration strategies, possible linked support resources, stateful volumes, risk reasons, and likely external requirements from redacted env var names such as `DATABASE_URL`, `REDIS_URL`, `MINIO_ENDPOINT`, or SMTP settings.

Review a migration plan:

```sh
bin/bort plan --manifest manifest.json --target dokploy
```

Filter a migration plan to a single app or migration role:

```sh
bin/bort plan --manifest manifest.json --app new-marketmap-dj
bin/bort plan --manifest manifest.json --role candidate
bin/bort plan --manifest manifest.json --role support
```

Export an inspectable migration bundle:

```sh
bin/bort export --manifest manifest.json --output-dir bort-bundle
```

Export only one app:

```sh
bin/bort export --manifest manifest.json --app my-app --output-dir bort-bundle
```

Validate an exported bundle:

```sh
bin/bort validate --bundle bort-bundle
```

Plan target-side preparation from an exported bundle without mutating the target:

```sh
bin/bort prepare --bundle bort-bundle --target dokploy
```

`bort prepare --format json` emits a versioned dry-run contract with structured target resource specs, Dokploy-specific render specs under `targetResources.dokploy`, heuristic linked-resource candidates, and readiness gates. The app shell can be ready to create while gates still require env input, resource decisions, or manual data-store review before migration proceeds.

Dry-run plan commands can persist their text or JSON output with `--output`. When `--format json` is used, the output is a local plan artifact that later stages can consume instead of recomputing every upstream stage from the bundle:

```sh
bin/bort prepare --bundle bort-bundle --target dokploy --format json --output prepare.json
bin/bort sync --from-prepare prepare.json --format json --output sync.json
bin/bort cutover --from-prepare prepare.json --from-sync sync.json --format json --output cutover.json
bin/bort rollback --from-cutover cutover.json
bin/bort commit --from-cutover cutover.json
```

Artifact consumption is opt-in. `sync` accepts `--from-prepare`; `cutover` accepts `--from-prepare` and can also accept `--from-sync` when the matching prepare artifact is supplied; `rollback` and `commit` accept `--from-cutover`. The CLI checks artifact API versions, dry-run metadata where present, bundle and target compatibility when those flags are supplied, and `--app` filters before building the next dry-run plan. `commit --from-cutover` uses the cutover artifact rollback window unless `--rollback-window` is supplied explicitly.

By default, environment variable values are redacted. If you need a full migration manifest for a trusted local workflow, opt in explicitly:

```sh
bin/bort scan --include-env-values --output manifest.json
```

Exported bundles are local artifacts with private directory and file permissions. When raw and resolved Coolify compose are both present, `bort export` uses raw compose; resolved compose is skipped unless it was explicitly included in the manifest, because it may contain interpolated secret values. Generated Docker bundles write service-specific env examples such as `.env.web.example` to avoid collisions between services. Each app bundle also includes `topology.json`, a machine-readable summary of networks, dependencies, external requirements, possible linked support resources, data stores, stateful volumes, routes, and risk reasons for later prepare/sync/cutover steps. The companion `migration-runbook.md` turns the same topology into a manual checklist for routes, environment, data stores, state sync, validation, cutover, and rollback readiness.

For the current safe audit loop, run only read-only/local commands:

```sh
bin/bort scan --source coolify --output coolify-manifest.json
bin/bort plan --manifest coolify-manifest.json --target dokploy
bin/bort export --manifest coolify-manifest.json --output-dir bort-bundle
bin/bort validate --bundle bort-bundle
bin/bort prepare --bundle bort-bundle --target dokploy
bin/bort sync --bundle bort-bundle --target dokploy
bin/bort cutover --bundle bort-bundle --target dokploy
bin/bort rollback --bundle bort-bundle --target dokploy
bin/bort commit --bundle bort-bundle --target dokploy
```

## Direction

The intended migration flow is:

```text
bort scan      # extract source platform state
bort plan      # classify apps as green/yellow/red
bort export    # write an inspectable local migration bundle
bort validate  # validate exported compose, env, routes, and storage
bort prepare   # plan target resources privately before creating anything
bort sync      # plan state copy or replication work
bort cutover   # plan traffic movement through the migration gateway
bort rollback  # plan route rollback to the source app
bort commit    # plan final target ownership acceptance
```

## Name

`bort` is named after Bortianor, a coastal town in Accra. The project is about safe passage: moving apps from one harbor to another without dropping traffic into the sea.
