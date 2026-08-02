# bort

<p align="center">
  <strong>move self-hosted apps between PaaS platforms without gambling on traffic, secrets, or data.</strong>
  <br />
  Coolify → Dokploy on the same VPS first. Reverse and cross-server migrations later.
  <br />
  <a href="#supported-path">Supported path</a>
  ·
  <a href="#quick-start">Quick start</a>
  ·
  <a href="#safety-model">Safety</a>
  ·
  <a href="#documentation">Documentation</a>
  ·
  <a href="#roadmap">Roadmap</a>
</p>

## About

Bort is a migration cockpit for people running their own application platform on
a VPS. It inventories what is actually running, explains what needs attention,
prepares the target privately, moves supported state, cuts traffic over, and
keeps every destructive boundary explicit.

The CLI is app-first rather than Docker-first: it shows readiness, generates
copy-paste fixes, persists review decisions, and tells the operator the next safe
action. Planning is dry-run by default. Host mutation begins only with an
explicit live apply.

## Supported path

The current product path is **Coolify → Dokploy on the same Linux VPS**.

| Capability | Status |
| --- | --- |
| Guided Coolify → Dokploy planning | Implemented |
| Explicit live apply and resume | Implemented |
| Target acceptance and source-container retirement | Implemented |
| Metadata cleanup and separately confirmed source purge | Implemented |
| Automated lifecycle trace with fake Docker and Dokploy | Implemented |
| Recorded disposable-host lifecycle acceptance | Still required before calling the live path production-proven |
| Executable rollback | Not implemented; Bort only prints the stored rollback plan |
| Dokploy → Coolify or cross-server migration | Not implemented |

Bort publishes macOS, Linux, and Windows binaries, but the complete same-VPS
live path depends on Linux host state, Docker, and privileged filesystem access.
Non-Linux builds should currently be treated as inspection and development
surfaces, not validated production migration hosts.

Today Bort can:

- discover local Coolify Docker state, local Docker, the Coolify API, or an
  existing manifest;
- export private application bundles containing Compose/image state, environment
  values, routes, storage, topology, and source-control metadata;
- record missing environment values and per-data-store migration strategies;
- prepare Dokploy projects and compose applications before explicit live apply;
- move supported local state with logical dump/restore or stopped-copy paths;
- cut traffic over with health checks, observation windows, resumable progress,
  and a stored rollback plan;
- accept the target only after successful live apply;
- inventory leftovers, narrowly clean eligible Dokploy metadata, and separately
  purge eligible source containers and networks after confirmation.

See the [migration guide](docs/migration-guide.md#current-workload-coverage) for
the workload-level coverage and limitations.

## Before you begin

Use a fresh VM or restorable snapshot while evaluating Bort. Do not test source
purge on a host containing data you intend to keep.

The lifecycle has two workspace rules:

1. Run every command from the **same working directory**. Bort stores its
   workspace under `.bort` relative to that directory.
2. Use the **same OS identity** throughout the migration. If the first command
   uses `sudo`, keep using `sudo`; if the first command does not, do not add it
   later.

Bort keeps workspace directories and files private, but `.bort` contains target
credentials and application configuration. Do not commit or publish it.

Read the [prerequisites and recovery guidance](docs/migration-guide.md) before a
live migration. In particular, Bort does not yet execute rollback.

## Install

Install from Homebrew:

```sh
brew install aikins01/tap/bort
```

Install from source with Go:

```sh
go install github.com/aikins01/bort/cmd/bort@latest
```

Or build the current checkout:

```sh
make build
```

The examples below use an installed `bort`; substitute `./bin/bort` for a local
build.

## Quick start

On the source VPS, create a dedicated workspace and start the guided cockpit:

```sh
mkdir -p ~/bort-migration
cd ~/bort-migration
sudo bort
```

The cockpit starts setup when needed and otherwise resumes the current run.
Review each application, follow its generated `fix:` and `next:` guidance, and
rerun `sudo bort` until the run shows `READY`.

For a noninteractive discovery start:

```sh
sudo bort migrate --source coolify-local
sudo bort
```

The migration lifecycle is deliberately explicit:

```text
sudo bort                 # start or resume, then review and fix
sudo bort migrate --live  # apply only the selected reviewed run
sudo bort rollback        # inspect the stored manual rollback plan
sudo bort commit --apply  # accept the target and retire source containers
sudo bort cleanup         # audit leftovers without deleting source resources
```

After cleanup inventory, `cleanup --apply` can remove only narrowly eligible
Dokploy metadata after a database backup. Destructive source cleanup remains a
separate dry-run-first command:

```sh
sudo bort cleanup purge --all-apps
```

Review its output and use the exact scoped apply command and confirmation phrase
Bort generates. Named volumes and host source paths are never deleted
automatically. Read the [cleanup and purge guide](docs/cleanup-and-purge.md)
before applying cleanup or purge.

## Safety model

Bort is designed for production boxes where the safest default is “look first.”

- **Dry-run first:** discovery, planning, validation, rollback inspection,
  acceptance planning, cleanup, and purge planning do not mutate the host.
- **Explicit live apply:** target creation and traffic movement only happen
  through `bort migrate --live` for an existing reviewed run.
- **Explicit run selection:** `.bort/state.json` identifies the current run;
  mutating commands do not select one by modification time.
- **Immutable live plans:** once live execution begins, changing the reviewed
  plan requires a new run.
- **Resumable serialized mutation:** run mutations share an operation lock, live
  apply records an attachable ledger, and read-only status remains available.
- **Private artifacts:** bundles, state, environment values, apply progress, and
  target credentials remain in the local workspace with private permissions.
- **Separate acceptance:** `commit --apply` retires source application containers
  only after successful live apply and is required before destructive source
  purge.
- **Separate destructive purge:** purge requires an explicit scope, successful
  live apply, accepted target, exact confirmation phrase, stable resource
  identities, and a private backup.
- **Manual rollback only:** `bort rollback` prints the stored plan. Bort does not
  currently execute recovery actions.

## Documentation

- [Migration guide](docs/migration-guide.md): prerequisites, workspace and
  identity rules, review, live apply, validation, acceptance, and recovery.
- [Cleanup and purge](docs/cleanup-and-purge.md): metadata cleanup, destructive
  scopes, manual prerequisites, lifecycle completion, and partial-failure
  recovery.
- [Lifecycle acceptance](docs/acceptance.md): automated fake trace,
  disposable-host matrix, destructive test procedure, and evidence handling.
- `bort help`: primary lifecycle commands.
- `bort help --advanced`: setup, automation, and artifact pipeline commands.

## Current limitations

- Rollback is inspectable but not executable.
- Continuous volume delta sync is not implemented.
- Database replication is limited to the current local logical dump/restore and
  stopped-copy strategies.
- GitHub Apps, deploy keys, webhooks, and equivalent source connections are
  recorded as metadata but are not copied, recreated, or revoked.
- Source image pruning is intentionally excluded because Docker layers and tags
  can be shared with target workloads.
- The full real-host acceptance matrix remains to be recorded on a disposable
  Linux VPS.

## Roadmap

Near-term work remains focused on making the same-VPS Coolify → Dokploy path
boring and safe before expanding the adapter surface:

1. record the disposable-host lifecycle acceptance matrix;
2. make workspace selection, prerequisite checks, and failure recovery clearer
   in the cockpit;
3. settle executable rollback semantics;
4. add Dokploy source scanning and Coolify target creation;
5. add cross-server transfer adapters after same-VPS lifecycle boundaries are
   proven.

Other Docker-, Compose-, and Swarm-shaped platforms remain research candidates.
Bort should only graduate an adapter when it can classify resources as safe,
blocked, or manual without hiding risk.

## Development and releases

Run the repository checks with:

```sh
make test
make vet
make build
```

The fake end-to-end trace and destructive disposable-host checklist are in the
[acceptance guide](docs/acceptance.md).

Releases are automated by GitHub Actions and GoReleaser. A new `v*` tag builds
macOS, Linux, and Windows archives, Linux packages, checksums, and the Homebrew
cask update. Use the intended release version rather than copying an old tag
from this README.

## Name

`bort` is named after Bortianor, a coastal town in Accra. The project is about
safe passage: moving applications from one harbor to another without dropping
traffic into the sea.
