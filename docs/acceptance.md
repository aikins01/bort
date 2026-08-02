# End-to-end acceptance testing

This guide is for maintainers testing Bort releases. It includes an automated
test with fake services and a destructive checklist for a disposable host. The
automated test does not prove that real Docker, proxy, volume, or filesystem
behavior works.

Do not claim production-host support until a redacted result from the
disposable-host checklist has been recorded for the version being released.

## Automated end-to-end test

The automated test uses a temporary workspace, fake Dokploy HTTP API, and fake
Docker command. It creates a self-contained run, performs live apply, verifies
`TARGET LIVE`, commits, verifies `COMMITTED`, inventories cleanup, applies a
confirmed all-app purge, and verifies `COMPLETE`:

```sh
go test ./internal/cli -run '^TestLifecycleAcceptanceTraceWithFakeDokploy$' -v
```

Run the complete repository checks separately:

```sh
make test
make vet
make build
```

## Disposable-host test cases

Use a fresh Linux VM or a restorable snapshot containing no data you intend to
keep. Exercise at least:

| Scenario | Required evidence |
| --- | --- |
| Stateless app | Domain remains healthy after live apply, acceptance, and source purge. |
| Database | Representative data survives the selected migration strategy and remains readable and writable on the target. |
| Named volume | Dry run requires manual completion; apply verifies absence without deleting resources automatically or recording completion. |
| Bind mount | Bort requires manual removal of the host path and refuses protected paths. |
| Interrupted live apply | Rerunning the command continues from saved progress without repeating completed work. |
| Two live commands | The second process shows the active progress while `status` remains readable. |
| Automatic purge | Bort removes the exact source containers and networks while keeping target resources and credentials. |
| Same-name replacement | Bort does not mistake a newly created resource for the one that was reviewed. |

## Disposable-host procedure

1. Install Coolify, then deploy one stateless disposable Coolify app with a
   route, source network, and non-secret test environment value. Do not perform a
   normal Dokploy installation alongside the active Coolify proxy. Use Bort's
   guided `same-VPS shadow mode`, its side-by-side Dokploy layout, so Dokploy's
   proxy is prepared without taking ports 80/443 before Bort switches web
   traffic.
2. Record the source container, network, and target project names before
   migration.
3. In a dedicated working directory, run the complete migration as one OS user:

   ```sh
   run="bort-acceptance-$(date +%Y%m%d-%H%M%S)"
   sudo bort migrate --source coolify-local --run "$run"
   sudo bort
   sudo bort migrate --live --run "$run"
   sudo bort status --run "$run"
   ```

4. Verify that the target serves the workload and wait through the rollback
   window recorded in the plan. Only then accept the target and plan source
   removal:

   ```sh
   sudo bort commit --apply --run "$run"
   sudo bort cleanup --run "$run"
   sudo bort cleanup purge --run "$run" --all-apps
   ```

5. Review the purge plan and verify the target still serves the disposable
   workload before applying it:

   ```sh
   sudo bort cleanup purge --apply --run "$run" --all-apps --confirm "purge $run"
   sudo bort status --run "$run"
   ```

6. Confirm that the target application remains healthy, `run.json` records live
   apply and acceptance, the selected non-platform source container and network
   are absent, and target resources plus source-control credentials remain. A
   real `coolify-local` run includes internal Coolify components that `--all-apps`
   excludes unless `--include-platform` is supplied, so this purge must not
   record `PurgedAt` or claim that the whole run is complete.
7. Restore the clean snapshot and repeat with a disposable app containing a
   named volume and a bind mount under a disposable directory. After the purge
   dry run, manually remove every listed source resource. Confirm that `--apply`
   performs only absence verification, leaves the run accepted rather than
   complete, and does not delete a replacement resource introduced before
   verification.
8. Repeat once with an interrupted live apply and once with two concurrent live
   invocations. Confirm that resume, attachment, and read-only status behave as
   documented.
9. Destroy the VM or restore its snapshot after retaining only the redacted
   evidence needed for the release.

## Evidence handling

Retain:

- Bort version and release candidate commit;
- host OS, architecture, Docker version, Coolify version, and Dokploy version;
- redacted command output and migration status;
- the test-case results;
- confirmation that the VM was destroyed or restored.

Do not retain or publish:

- `.bort/state.json`;
- exported environment values;
- Coolify or Dokploy tokens;
- database dumps or application data;
- unredacted manifests, bundles, apply ledgers, or purge backups.
