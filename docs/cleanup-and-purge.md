# Cleanup and purge

Cleanup normally happens after live apply succeeds and you accept Dokploy. Bort
keeps two commands separate:

- `cleanup` lists leftovers and can remove a small set of eligible unused
  records from the Dokploy database;
- `cleanup purge` removes source resources only after you select the apps or
  projects and confirm the command.

Neither command removes target Dokploy applications or source-control
credentials.

## Accept the target first

Validate the target and wait through the `rollback window` recorded in the plan
before running:

```sh
sudo bort commit --apply
```

This confirms that Dokploy is now the active platform and stops the source
application containers. Bort does not currently provide automatic rollback, so
do not run it until you have independently checked the target. Bort does not
measure the rollback window or block this command when the window has not passed.

## Ordinary cleanup

Inventory leftovers with:

```sh
sudo bort cleanup
```

This command only shows what it found. It reports source containers, volumes,
networks, source-control details, resources created in Dokploy, and eligible
unused records in the Dokploy database.

Remove only those eligible Dokploy records with:

```sh
sudo bort cleanup --apply
```

Before changing the Dokploy database, Bort backs it up. The command only removes
eligible empty records with no domains from Dokploy projects named
`coolify-proxy`, `proxy`, or `source`. It does not remove source containers,
volumes, networks, paths, credentials, Docker images, or target applications.

## Plan a source purge

Start with a preview, which Bort calls a dry run:

```sh
sudo bort cleanup purge
```

Select the apps or projects you intend to remove before applying:

```sh
sudo bort cleanup purge --app demo-app
sudo bort cleanup purge --project demo-project
sudo bort cleanup purge --all-apps
```

`--all-apps` covers normal applications by default. Internal Coolify components,
such as its source and proxy services, additionally require
`--include-platform`.

Review every listed resource and use the exact apply command and confirmation
phrase emitted by the dry run. To select a named run, replace `RUN_NAME` in this
example:

```sh
run="RUN_NAME"
sudo bort cleanup purge --run "$run" --all-apps
sudo bort cleanup purge --apply --run "$run" --all-apps --confirm "purge $run"
```

Purge apply refuses to run unless:

- `--app`, `--project`, or `--all-apps` selects what to remove;
- Bort's saved record shows that live apply succeeded;
- the target was accepted with `commit --apply`;
- the exact `purge <run-name>` confirmation is supplied.

Bort writes a private purge-plan backup under `.bort/backups` before destructive
work begins.

## Automatic removal and manual-removal checks

Bort behaves differently depending on the selected resources.

### Eligible containers and networks

When none of the selected resources requires manual removal, Bort can remove
eligible source containers and networks. It records and rechecks their exact
Docker IDs so it will not delete a different resource that happens to reuse a
name.

### Named volumes or host paths

Bort never automatically deletes named volumes or host source paths. If the
selection contains either one, the preview requires you to remove every listed
source container, volume, network, and path yourself.

After those resources have been removed manually, `cleanup purge --apply` only
verifies that everything you selected remains absent. It does not mix manual
removal with automatic deletion.

Verification-only purge does not set the run's `PurgedAt` timestamp because Bort
cannot prevent an external tool from recreating a resource after the absence
check.

## When the migration is marked complete

A successful automatic purge covering all run applications can mark the
migration complete. Removing one app or project does not claim completion for
the whole run. A manual-removal check also leaves the run accepted rather than
marking the whole migration complete.

Check the resulting state with:

```sh
sudo bort status
```

## What purge preserves

Source purge does not remove:

- target Dokploy projects, compose applications, domains, or volumes;
- GitHub Apps, deploy keys, webhooks, or other source-control credentials;
- Docker images, because image layers and tags can be shared with target
  workloads;
- named volumes or host paths automatically.

## Recover from a partial purge

A destructive operation can stop after some earlier resources have already been
removed. If purge reports an incomplete result:

1. Stop issuing cleanup commands.
2. Save the complete output without publishing secrets.
3. Inspect the purge results and the private backup path printed by Bort.
4. Verify the target still serves the application.
5. Compare every selected source resource with the recorded result before
   retrying the generated scoped command.

Do not recreate a source resource under the same name and assume it is the same
object. Bort checks Docker IDs and refuses a different replacement resource.

While evaluating Bort, exercise destructive cleanup only on a fresh VM or
restorable snapshot containing no data you intend to keep.
