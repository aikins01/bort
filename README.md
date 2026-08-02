# bort

<p align="center">
  <strong>move self-hosted apps between platforms without gambling on traffic, secrets, or data.</strong>
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

Bort is a guided migration tool for people running their own app platform on a
VPS. It checks what is actually running, explains what needs attention, prepares
Dokploy, copies supported data, switches web traffic, and requires an explicit
command before each destructive step.

Bort organizes the migration by app rather than exposing a wall of Docker
details. It shows what is ready, provides copy-paste fixes, saves your answers,
and tells you what to do next. Discovery and planning only preview app changes.
If Dokploy is not prepared, the `migrate --live` preflight can offer to install
it before application migration begins. Bort explains the server changes and
asks first. Moving apps, data, and traffic begins only when you explicitly run
the live command.

## Supported path

The current product path is **Coolify → Dokploy on the same Linux VPS**.

| Capability | Status |
| --- | --- |
| Guided Coolify → Dokploy planning | Implemented |
| Explicit live apply and resume | Implemented |
| Accepting Dokploy and stopping source containers | Implemented |
| Safe Dokploy database cleanup and separately confirmed source removal | Implemented |
| Executable rollback | Not implemented; Bort only prints the stored rollback plan |
| Dokploy → Coolify or cross-server migration | Not implemented |

Bort publishes macOS, Linux, and Windows binaries, but a complete same-VPS move
depends on Linux, Docker, and access to protected host files. Use the non-Linux
builds to inspect migration files and help develop Bort, not to run a production
migration.

Today Bort can:

- discover local Coolify Docker state, local Docker, the Coolify API, or an
  existing manifest;
- export private application bundles containing Compose/image details,
  environment values, routes, storage, Docker networks, and source-control
  details;
- record missing environment values and how each database or storage service
  should be moved;
- prepare Dokploy projects and compose applications before explicit live apply;
- move supported local data by exporting and importing databases or copying data
  while a service is stopped;
- switch web traffic during explicit live apply, save progress for retries, and
  store rollback instructions;
- accept the target only after successful live apply;
- list leftovers, remove only eligible unused records from Dokploy, and
  separately remove eligible source containers and networks after confirmation.

See the [migration guide](docs/migration-guide.md#current-workload-coverage) for
the workload-level coverage and limitations.

## Before you begin

Use a fresh VM or restorable snapshot while evaluating Bort. Do not test source
purge on a host containing data you intend to keep.

Follow two workspace rules during the migration:

1. Run every command from the **same working directory**. Bort stores its
   workspace under `.bort` relative to that directory.
2. Use the **same OS user** throughout the migration. If the first command
   uses `sudo`, keep using `sudo`; if the first command does not, do not add it
   later.

Bort keeps workspace directories and files private, but `.bort` contains target
credentials and application configuration. Do not commit or publish it.

Read the [prerequisites and recovery guidance](docs/migration-guide.md) before a
live migration. In particular, Bort does not yet execute rollback.

## Install

Install from Homebrew:

```sh
brew install --cask aikins01/tap/bort
```

Official archives and Linux packages are available from
[GitHub Releases](https://github.com/aikins01/bort/releases).

Build the current checkout from source with:

```sh
make build
```

The examples below use an installed `bort`; substitute `./bin/bort` for a local
build.

## Quick start

On the source VPS, create a dedicated workspace and start Bort:

```sh
mkdir -p ~/bort-migration
cd ~/bort-migration
sudo bort
```

The guided screen starts setup when needed and otherwise resumes the current
run. Review each application, follow its generated `fix:` and `next:` guidance,
and rerun `sudo bort` until the run shows `READY`. This means blocking inputs
are resolved; review any remaining non-blocking notes before live apply.

To start discovery directly without the setup questions:

```sh
sudo bort migrate --source coolify-local
sudo bort
```

The migration uses separate commands for each important step:

```text
sudo bort                 # start or resume, then review and fix
sudo bort migrate --live  # apply only the selected planned run
sudo bort rollback        # inspect the stored manual rollback plan
sudo bort commit --apply  # accept the target and retire source containers
sudo bort cleanup         # audit leftovers without deleting source resources
```

After reviewing the cleanup results, `cleanup --apply` can remove only eligible
unused records from the Dokploy database, and only after making a database
backup. Removing source resources remains a separate command that previews its
work first:

```sh
sudo bort cleanup purge --all-apps
```

Review its output and use the exact apply command and confirmation phrase
Bort generates. Named volumes and host source paths are never deleted
automatically. Read the [cleanup and purge guide](docs/cleanup-and-purge.md)
before applying cleanup or purge.

## Safety model

Bort's safety model defaults to “look first.”

- **Preview first:** discovery, planning, validation, rollback inspection,
  acceptance planning, cleanup, and purge planning do not change the server.
- **Confirmed Dokploy installation:** the `migrate --live` preflight may offer to
  install Dokploy in Bort's side-by-side same-VPS layout. It warns that this
  writes system configuration, creates Docker resources, initializes Swarm when
  needed, and may disable Docker live-restore and reload Docker before asking
  for confirmation. Application migration begins only after confirmation.
- **Explicit live apply:** creating target apps, copying data, and moving traffic
  only happen through `bort migrate --live` for an existing planned run.
- **Known current run:** `.bort/state.json` identifies the current run. Commands
  that make changes do not guess based on which file was modified most recently.
- **Plans are locked during live work:** once live execution begins, changing
  the selected plan requires a new run.
- **One change at a time:** Bort prevents two commands from changing the same run
  at once, saves live progress for retries, and keeps `status` available.
- **Private files:** bundles, state, environment values, live progress, and
  target credentials stay in the local workspace with private permissions.
- **Separate acceptance:** `commit --apply` retires source application containers
  only after successful live apply and is required before destructive source
  purge.
- **Separate destructive purge:** purge requires selected apps or projects, a
  successful live apply, an accepted target, the exact confirmation phrase, a
  recheck of Docker IDs, and a private backup.
- **Manual rollback only:** `bort rollback` prints the stored plan. Bort does not
  currently execute recovery actions.

## Documentation

- [Migration guide](docs/migration-guide.md): requirements, workspace and user
  rules, review, live apply, validation, acceptance, and recovery.
- [Cleanup and purge](docs/cleanup-and-purge.md): safe Dokploy cleanup, source
  removal, manual steps, completion, and recovery from a partial failure.
- `bort help`: main migration commands.
- `bort help --advanced`: setup, automation, and migration-file commands.

## Current limitations

- Bort shows rollback instructions but does not run them.
- Bort does not keep copying new volume changes while applications remain live.
- Database moves currently use local export/import or copy data while the
  database is stopped.
- GitHub Apps, deploy keys, webhooks, and equivalent source connections are
  recorded as details but are not copied, recreated, or revoked.
- Bort does not automatically delete source Docker images because the target may
  share their layers and tags.

## Roadmap

Near-term work remains focused on making the same-VPS Coolify → Dokploy path
boring and safe before adding more platforms:

1. make workspace selection, requirement checks, and failure recovery clearer
   in the guided screen;
2. decide how automated rollback should work;
3. add Dokploy source scanning and Coolify target creation;
4. add cross-server transfers after the same-VPS steps are proven.

Other Docker-, Compose-, and Swarm-based platforms remain possible future
targets. Bort should only support one when it can clearly say which resources
are safe to move, which are blocked, and which need manual work.

## Development

Run the repository checks with:

```sh
make test
make vet
make build
```

## Name

`bort` is named after Bortianor, a coastal town in Accra. The project is about
safe passage: moving applications from one harbor to another without dropping
traffic into the sea.
