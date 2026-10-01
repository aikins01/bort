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
Dokploy, moves named volumes and Postgres dumps into Bort-owned volumes before
the target deploys, switches web traffic, and requires an explicit command
before each destructive step.

Bort organizes the migration by app rather than exposing a wall of Docker
details. It shows what is ready, provides copy-paste fixes, saves your answers,
and tells you what to do next. Discovery and planning only preview app changes.
If Dokploy is not prepared, the `migrate --live` preflight can offer to install
it before application migration begins. Bort explains the server changes and
asks first. Creating target apps, transferring persistent state, and moving
traffic begin only when you explicitly run the live command.

## Supported path

The current product path is **Coolify → Dokploy on the same Linux VPS**.

| Capability | Status |
| --- | --- |
| Guided Coolify → Dokploy planning | Implemented |
| Explicit live apply and resume | Implemented |
| Accepting Dokploy and stopping source containers (sources without an orchestrator) | Implemented |
| Accepting Dokploy for a Coolify source | Manual source retirement, then recorded with the exact `bort recover-authority --run <run> --authority target --source-retired --confirm '...'` command Bort prints |
| Safe Dokploy database cleanup and separately confirmed source removal | Implemented |
| Stateless traffic rollback | Implemented (`bort rollback --live`) |
| Stateful live apply (named volumes and Postgres dump/restore) | Implemented; state is staged into Bort-owned volumes before the target deploys |
| Bind-mount transfer for stateful apps | Not implemented; same-host bind mounts that keep their paths need no transfer |
| Automatic stateful rollback | Not available; manual authority recovery is required |
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
- copy named volumes and Postgres dumps into Bort-owned staging volumes during
  live apply, before the target deploys, and identify the state that must be
  migrated outside Bort, such as bind-mount copies;
- switch web traffic during explicit live apply, save progress for retries, and
  store rollback instructions;
- execute a reviewed stateless rollback (`bort rollback --live`) to return web
  traffic to the source;
- accept the target only after successful live apply;
- list leftovers, remove only eligible unused records from Dokploy, and
  separately remove eligible source containers and networks after confirmation.

See the [migration guide](docs/migration-guide.md#current-workload-coverage) for
the workload-level coverage and limitations.

## Before you begin

Use a fresh VM or restorable snapshot while evaluating Bort. Do not test source
purge on a host containing data you intend to keep.

Follow two state-management rules during the migration:

1. Run every command from the **same working directory**. Bort stores its
   workspace under `.bort` relative to that directory.
2. Use the **same OS user** throughout the migration. If the first command
   uses `sudo`, keep using `sudo`; if the first command does not, do not add it
   later. Bort also stores its host-wide operation lock and durable Dokploy
   traffic owner under `/var/lib/bort`.

Bort keeps these directories and files private. `.bort` contains target
credentials and application configuration; `/var/lib/bort` binds shared
Dokploy traffic to one migration run. Preserve both, including the original
Dokploy credential, through any `cleanup --apply` you intend to run. Do not
commit or publish either directory.

Read the [prerequisites and recovery guidance](docs/migration-guide.md) before a
live migration. In particular, review how `bort rollback --live` works before you
need it.

For a Dokploy install, prepare an absolute authentication-secret escrow path on
encrypted or off-host storage, inside a mode-0700 directory owned by root or
the invoking sudo user. No directory in the path may be a symbolic link. Every
parent must be owned by root or that user; a parent writable by group or others
must have the sticky bit set. Pass the path as `--auth-secret-backup` or as
`sudo BORT_DOKPLOY_AUTH_SECRET_BACKUP=/path bort ...` (sudo drops exported
variables by default), and retain it with the private provenance marker under
`/var/lib/bort`. Interrupted creation is retried only when the
retained escrow, private creation intent, and Docker secret label still match.

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
and rerun `sudo bort` until the run shows `READY` or reports `MANUAL STATE`.
`READY` means blocking inputs are resolved; review any remaining non-blocking
notes before live apply. `MANUAL STATE` means a historical stateful run was
applied with an older plan version that copied state into an already deployed
target. Bort cannot continue that run; follow the displayed manual recovery
guidance or create a new run. `PLAN BLOCKED` means live apply would refuse the
planned state transfer (for example a bind mount or a named volume shared by
two apps); choose `bort data <app> <store> --recreate` or `--managed` for a data
store volume and re-plan, or change the source compose and scan a new run.
`NEW RUN REQUIRED` means live apply refused this run at `pause_source` and the
run cannot be re-planned; follow the recovery `bort status` shows, then create
a new run.

To start discovery directly without the setup questions:

```sh
sudo bort migrate --source coolify-local
sudo bort
```

The migration uses separate commands for each important step:

```text
sudo bort                 # start or resume, then review and fix
sudo bort migrate --live  # apply only the selected planned run
sudo bort rollback        # inspect the stored rollback plan
sudo bort rollback --live # return stateless traffic when safe
sudo bort commit --apply  # accept the target when no source orchestrator can recreate it
sudo bort cleanup         # audit leftovers without deleting source resources
```

For a Coolify source, keep the Coolify control plane stopped, remove the
reviewed source app and proxy containers with Docker, and record permanent
target authority with the exact
`recover-authority --authority target --source-retired` command Bort prints.
Stopping Coolify pauses it for every app on the host. Leave it stopped
afterwards when nothing else needs it, because starting it again can recreate
its proxy and redeploy the removed apps; otherwise start it and immediately
delete the migrated apps in Coolify. `commit --apply` refuses because stopping
containers cannot fence Coolify itself.

For owner-bound current runs, `cleanup --apply` can remove only eligible unused
records from the bound local Dokploy database, and only after making a database
backup. Pre-upgrade runs without that durable binding are audit-only. Removing
source resources remains a separate command that previews its work first:

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
- **Explicit live apply:** creating target apps and moving traffic only happen
  through `bort migrate --live` for an existing planned run.
- **State moves before the target exists:** for apps with named volumes or a
  Postgres database, Bort stops the source app, copies each volume and restores
  each dump into volumes it owns and labels with the run name, verifies the
  source stayed stopped, and keeps one read-only `bort-pin-*` container attached
  to those volumes until the Dokploy target mounts them exactly. Bort does not
  deploy the compose until state is staged and refuses unexpected attachments;
  do not deploy the app manually in Dokploy during live apply. If handoff is
  unsafe, Bort keeps the source stopped, reports whether it reverified the pin,
  and directs recovery through `bort status`. A retryable failure before
  handoff can restart the source, but keeps the pin for the next attempt. Do not
  remove a `bort-pin-*` container manually. Source recovery checks that no
  target container is attached, even if a pin is already absent. Final target
  acceptance requires a durable record of the target's mounts and removes a
  remaining pin only when the target holds exactly those attachments.
  The source stays stopped while the target becomes the writer, including for
  unrouted apps.
- **Coolify stays fenced during stateful moves:** before a stateful live apply
  from Coolify, let in-progress Coolify deployments finish, record the
  `coolify` container's restart policy with
  `sudo docker inspect --format '{{.HostConfig.RestartPolicy.Name}}' coolify`,
  then run
  `sudo docker update --restart=no coolify && sudo docker stop coolify` so
  Coolify cannot redeploy a paused source app. Bort refuses to start, pause a
  source, deploy a target, or move routes while the `coolify` container is
  running or would restart, or while a Coolify deployment helper is still
  running deployment commands. Stopping it pauses Coolify for every app on the
  host. Restart Coolify only after source authority is finalized; after target
  acceptance, follow the restart guidance in the
  [migration guide](docs/migration-guide.md#apply-the-selected-run).
- **Known current run:** `.bort/state.json` identifies the current run. Commands
  that make changes do not guess based on which file was modified most recently.
- **Plans are locked during live work:** once live execution begins, changing
  the selected plan requires a new run.
- **Attested source host:** local discovery records the Docker Engine ID and
  exact source container identities. Automated source mutation during live
  apply, recovery, rollback, and commit refuses a different daemon, replaced
  containers, imported manifests or bundles, and legacy runs without that
  attestation. Those runs remain inspectable but require manual source handling
  or a new local scan for live work. Runtime checks catch a replaced daemon or
  container, not every stale detail in a reviewed configuration.
- **One change at a time:** Bort prevents two commands from changing the same run
  at once, saves live progress for retries, and keeps `status` available.
- **Private files:** bundles, environment values, live progress, and target
  credentials stay in the local workspace. The host-wide operation lock and
  durable Dokploy traffic owner stay under `/var/lib/bort`. Both locations use
  private permissions.
- **Separate acceptance:** `commit --apply` retires source application containers
  only when no external orchestrator can recreate them. It refuses Coolify
  sources. With the Coolify control plane stopped, remove the reviewed source
  app and proxy containers, verify they stay removed, then use confirmed
  target-authority recovery with `--source-retired` to record acceptance.
  Afterwards, restart Coolify only if other apps need it, and then delete the
  migrated apps in Coolify immediately.
  Destructive source purge requires either accepted state.
- **Separate destructive purge:** purge requires selected apps or projects, a
  successful live apply or completed manual target-authority
  recovery, an accepted target, the exact confirmation phrase, a recheck of
  Docker IDs, and a private backup.
- **Fail-closed rollback:** `bort rollback --live` can swap traffic back only
  when the run transferred no database or volume state. Dokploy v0.30.7 cannot
  durably block queued or future deployments, so Bort refuses automatic
  stateful rollback before changing either side. Preserve both sides and
  establish writer and traffic authority manually if a stateful target fails
  validation. When the durable host owner still belongs to the run,
  `bort recover-authority` records the verified source or target decision and
  finishes that run without abandoning its ownership record.
  Rollback inspection reports `automaticAvailable` and `automaticBlocker` in
  JSON and the equivalent reason in text before any mutation.

## Documentation

- [Migration guide](docs/migration-guide.md): requirements, workspace and user
  rules, review, live apply, validation, acceptance, and recovery.
- [Cleanup and purge](docs/cleanup-and-purge.md): safe Dokploy cleanup, source
  removal, manual steps, completion, and recovery from a partial failure.
- `bort help`: main migration commands.
- `bort help --advanced`: setup, automation, and migration-file commands.

## Current limitations

- Stateful live apply transfers named volumes and Postgres dumps only. A
  stateful app whose plan needs a bind-mount copy is refused; same-host bind
  mounts that stay at their existing paths do not require a transfer.
- A named volume mounted by more than one compose service cannot be staged.
  Live apply refuses it before changing Dokploy; choose `bort data <app>
  <store> --recreate` or `--managed` for a data store volume and re-plan with
  `bort migrate --run <run>`, or change the source compose and scan a new run.
- A migrated Postgres store must keep its data directory (`PGDATA`) on a named
  volume the service mounts, with no writable bind mount inside it and no named
  volume outside it. A logical dump cannot restore an auxiliary volume such as
  `/backups`.
  Live apply refuses other layouts before the source pauses (a run already
  past that app's `pause_source` when this check was added refuses at the
  restore step and restarts the source during cleanup). When the service
  mounts no named volume, sets `PGDATA` in its compose `environment` to a
  literal path that breaks this rule, interpolates a mount target, or declares
  Compose `secrets` or `configs`, Bort refuses before live apply starts (`PLAN
  BLOCKED`): choose `bort data <app> <store> --recreate` or `--managed` and
  re-plan with `bort migrate --run <run>`, or change the source compose and
  scan a new run. When the image or interpolation decides `PGDATA`, only the
  created staged container reveals the directory, so the refusal comes at
  `pause_source` (`NEW RUN REQUIRED`) with that app's source still running and
  none of its state transferred, and the run cannot be re-planned: follow the
  recovery `bort status` shows (releasing the run when no other app's source
  was paused or handed off), then choose a strategy or change the source
  compose and create a new run.
- Runs created by older Bort versions whose plan copied state into an already
  deployed target remain refused (`MANUAL STATE`). Dokploy v0.30.7 cannot
  durably prevent queued or future deployments from restarting a target writer
  during such a copy. Create a new run instead.
- Automatic rollback is limited to stateless traffic handoff. Stateful rollback
  requires manual writer and traffic authority recovery.
- The temporary compose project Bort uses to restore a database writes its
  `.env` in the format the running Dokploy release uses, read from
  `settings.getDokployVersion` before the source is paused. Live apply refuses
  staged restores when that version is not a `vMAJOR.MINOR.PATCH` release.
- GitHub Apps, deploy keys, webhooks, and equivalent source connections are
  recorded as details but are not copied, recreated, or revoked.
- Bort does not automatically delete source Docker images because the target may
  share their layers and tags.

## Roadmap

Near-term work remains focused on making the same-VPS Coolify → Dokploy path
boring and safe before adding more platforms:

1. add Dokploy source scanning and Coolify target creation;
2. add cross-server transfers after the same-VPS steps are proven.

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
