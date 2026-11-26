# migrations

**TL;DR:** Schema changes for the shared `orders` database are numbered SQL files in `sql/`. A Kubernetes Job in `apps` applies the pending ones with [golang-migrate](https://github.com/golang-migrate/migrate), under a Postgres advisory lock, then exits. It connects as `app`, the role that owns the database and that the services use. Argo CD runs the Job as a Sync hook in its own `migrations` Application, in the wave before the services. Postgres starts empty every morning, so at bootstrap the root holds the services back until the Job succeeds. Day to day, merge a migration before the release that needs it. Changes are expand/contract and roll forward: there are no down migrations. The design is in [Database migrations](../../docs/architecture-decisions.md#database-migrations).

Filled by P5-04 (#58) and P5-05 (#59). Moved under Argo CD by P7-04 (#76).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `external-secret.yaml` | `ExternalSecret` | `db-migrate` | Copies Key Vault `postgres-password`, the `app` role's password, into the Secret `db-migrate`, key `PGPASSWORD`. Pods in `apps` can't read `postgres-credentials` in `data`. Syncs once, then only on `force-sync`, like the services' copies ([data](../data/README.md#rotating-a-password)) |
| `kustomization.yaml` | `ConfigMap` (generated) | `db-migrate-<hash>` | Every file in `sql/`, mounted into the Job at `/migrations` |
| `job.yaml` | `Job` | `db-migrate` | Runs `migrate up` once and exits. An Argo CD Sync hook |
| `networkpolicy.yaml` | `NetworkPolicy` | `db-migrate` | Out to Postgres on 5432 only. `apps` denies everything else ([namespaces](../namespaces/README.md#networkpolicies)) |

The SQL ships in a ConfigMap, not an image, so a migration needs no CI build or registry push. A ConfigMap holds up to 1 MiB, far more than this schema needs. Its name carries a hash of the SQL, which makes Argo CD run a new migration ([Under Argo CD](#under-argo-cd)).

## How a run works

1. Argo CD syncs the `migrations` Application. It applies the ConfigMap, the ExternalSecret and the NetworkPolicy, and waits for them to be Healthy: the ExternalSecret must have synced the password.
2. In the next wave it deletes any earlier `db-migrate` Job and creates a new one (`BeforeHookCreation`). A finished Job of the same name would make the run a no-op, for example after a database reset.
3. The Job's pod reads the password from the `db-migrate` Secret as `PGPASSWORD`. The driver, `lib/pq`, reads that variable, so the password needs no escaping. An ESO-templated URL would break on a password containing `@`, `:` or `/`.
4. golang-migrate takes a Postgres advisory lock. A second run waits up to 60 seconds (`-lock-timeout=60`) for the first to finish, then finds nothing to do.
5. It reads the version from `schema_migrations` and applies each newer file in order. Each file runs as one implicit transaction, so a failing file leaves no partial change.
6. Each file starts with `SET lock_timeout = '10s';`. A DDL statement that can't get its table lock in 10 seconds fails, so it doesn't block the services' queries. The setting is in the files, not the connection URL, because in the URL it would also cut the advisory lock wait to 10 seconds.
7. The pod exits 0, the Job completes and the sync succeeds. A second run with nothing new reports `no change`.

`backoffLimit: 2` retries failures before any file runs, such as Postgres restarting or the advisory lock not freed within 60 seconds. It can't recover a failure inside a file. golang-migrate marks the version dirty before running a file, so every retry stops at `Dirty database version N` (see [When a migration fails](#when-a-migration-fails)). A Job that fails three times, or runs past `activeDeadlineSeconds` (5 minutes), ends `Failed`, and so does the sync.

## Under Argo CD

The Job sits in its own Application, `migrations`, at wave 8, one wave before the nine services ([argocd](../../argocd/README.md#how-the-cluster-comes-up)). Inside it, the ConfigMap, the ExternalSecret and the NetworkPolicy are ordinary resources at wave 0. The Job is a Sync hook at wave 1 with `BeforeHookCreation`.

**Why not in the services' Applications.** A hook only gates its own Application's sync, and each service is its own Application. Gating every Postgres user would take seven copies of the Job under seven names, each run but the first waiting on the advisory lock. One Application runs it once.

**At bootstrap** the root waits for each wave to pass its health check, which passes `migrations` only once its last sync has Succeeded: the Job has completed, not just its inputs turned Healthy. A running or retrying Job holds wave 9, and a failed one fails the root's wave 8, so the services never start on an unmigrated or broken schema. The root retries every 3 minutes at most until the wave passes ([When a sync fails](../../argocd/README.md#when-a-sync-fails)).

`migrations` itself retries only twice, unlike the other Applications. Rerunning a failed file only marks the version dirty again, so it waits for you.

**Day to day** each Application syncs on its own when its folder changes, so nothing orders a migration before a service in the same merge. The rule: **merge a migration before the release that needs it.** Expand/contract makes that safe, since every migration must already work with the running release ([Database migrations](../../docs/architecture-decisions.md#database-migrations)).

**Why the inputs aren't hooks.** Argo CD leaves hooks out of an Application's sync status, and syncs automatically only when that status is OutOfSync. An Application made only of hooks always reads Synced and never syncs, even on a fresh cluster (Argo CD v3.6.1, `controller/state.go` and `autoSync` in `controller/appcontroller.go`). So the inputs are ordinary resources, and the ConfigMap name carries the hash of the SQL:

- On a fresh cluster they are missing, so the Application is OutOfSync and syncs.
- A new file changes the hash, so the new ConfigMap is missing and the Application syncs. The Job mounts the new name, and the old ConfigMap is pruned.
- An unrelated sync, such as selfHeal putting back an edited NetworkPolicy, runs the Job too. golang-migrate reports `no change`.

To run it with no change in Git, for example after resetting the database by hand: `argocd app sync migrations`.

## What the services do today

The five services that own tables (order, inventory, payment, notification and shipping) create them at startup in `migrate()`, with `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS`. notification-service also seeds seven templates with `ON CONFLICT DO NOTHING`. scheduler and dashboard-api read those tables but create nothing.

`sql/000001_baseline.up.sql` is all five `migrate()` functions copied verbatim. Run before or after the services start, it gives the same schema: `pg_dump -s` of the two orders is identical (see [Validation](#validation)). So both can run safely:

- On an empty database, whichever runs first creates the tables and the other skips them.
- On a database the services already built, the baseline is a no-op and records version 1.

Two rules keep this safe until the services' `migrate()` functions are removed:

- **Never edit `migrate()` in a service again.** Put every schema change in a new file here, or the two sources drift.
- **Run the Job as the role the services connect as, `app`.** `CREATE INDEX IF NOT EXISTS` needs table ownership even when the index exists. A service connecting as a role that doesn't own the tables exits at startup with `must be owner of table orders`. Tables the superuser created would belong to `postgres`, so the Job never uses it. The role is set in `DATABASE_URL` in `job.yaml`.

Removing `migrate()` from the services is a follow-up. It also stops each pod start taking a brief `SHARE` lock on every table, which `CREATE INDEX IF NOT EXISTS` takes even when the index exists.

## Add a migration

1. Add `sql/<next number>_<what>.up.sql`, six digits, one more than the last: `000002_orders_add_gift_message.up.sql`.
2. Start it with `SET lock_timeout = '10s';`.
3. List it under `configMapGenerator.files` in `kustomization.yaml`. Kustomize has no glob.
4. Keep the change backward compatible with the running release: see [expand and contract](../../docs/architecture-decisions.md#database-migrations). Write nothing that only the new release can live with.
5. `CREATE INDEX CONCURRENTLY` can't run inside a transaction, so put it in its own file, as the only statement, without the `SET` line.
6. Merge it on its own, before the tag bump of any release that needs it. Wait for `migrations` to show Synced and Healthy.

Never edit or renumber a file that has been applied anywhere. golang-migrate records only the version number, so an edit to an applied file never runs.

## When a migration fails

golang-migrate marks the version dirty before running a file. The file's transaction rolled back, so the schema is unchanged, but every later run stops with `Dirty database version N. Fix and force version.`

1. Find the failed version. Argo CD recreates the hook Job on each retry, so the Job's log may be from a later run that stopped at `Dirty database version N`, and the file's own error may be gone. The `schema_migrations` row says which file failed: N is the version marked `t`.

   ```bash
   kubectl --context aks-v2-platform -n argocd get application migrations -o jsonpath='{.status.operationState.message}{"\n"}'
   kubectl --context aks-v2-platform logs -n apps job/db-migrate
   kubectl --context aks-v2-platform exec -n data postgres-0 -- \
     psql -U postgres -d orders -tAc "SELECT version, dirty FROM schema_migrations"
   ```

2. Clear the dirty version. What to run depends on which file failed:

   - **A later file, N of 2 or more.** Set the version back to N-1, the last file that applied:

     ```bash
     kubectl --context aks-v2-platform exec -n data postgres-0 -- \
       psql -U postgres -d orders -c "UPDATE schema_migrations SET version = <N-1>, dirty = false"
     ```

   - **The baseline, N = 1.** Delete the row. Version 0 isn't a migration, so setting it fails every later run with `no migration found for version 0`:

     ```bash
     kubectl --context aks-v2-platform exec -n data postgres-0 -- \
       psql -U postgres -d orders -c "DELETE FROM schema_migrations"
     ```

3. Fix the file and merge the fix. It never applied, so editing it is safe. The new hash makes Argo CD sync `migrations` and replace the failed Job.

Clear the version before the fix merges, or the new Job stops at the dirty version too. If it did, clear it and run `argocd app sync migrations`.

At bootstrap, the root keeps retrying wave 8 every 3 minutes at most. Once `migrations` has synced and is Healthy, its next retry starts the services. To start them now: `argocd app terminate-op root`, then `argocd app sync root`.

The services were never rolled out on a migration that failed at bootstrap. Day to day, the rule above means they are still on the release that matches the old schema.

## Validation

Offline, from the repo root:

```bash
kustomize build k8s/migrations | kubeconform -strict -ignore-missing-schemas -summary
```

With Docker, against a throwaway `postgres:16`, running the pinned image as the Job does (user 65532, read-only root, no capabilities, migrations mounted through `..data` symlinks like a ConfigMap volume, connecting as an `app` role that owns the database):

| Check | Result |
| --- | --- |
| Baseline on an empty database | `1/u baseline`, 10 tables owned by `app`, 7 templates |
| Run again | `no change` |
| A 30-second migration, and a second run started 3 seconds in | The second waited 27 seconds for the advisory lock, then `no change` |
| An `ALTER TABLE` behind an open transaction | Failed after 10 seconds with `lock timeout` instead of waiting |
| A retry after that failure, as `backoffLimit` would run it | `Dirty database version 3` |
| A file whose second statement fails | Nothing from the file applied, version marked dirty |
| Recovery of a later file: version set to N-1 | The fixed file applied |
| A failed baseline, version set to 0 | `no migration found for version 0` |
| A failed baseline, row deleted | The fixed baseline applied |
| Database reset, then run again | `1/u baseline` again |
| Password with `@`, `:`, `/` and `#` in `PGPASSWORD` only | Connected |
| The five services' own `migrate()` on an empty database, versus the baseline | `pg_dump -s` identical |
| The five services starting as `app` on a database the baseline built | All five migrated and started |
| The services connecting as a role that doesn't own the tables | All five exited: `must be owner of table` |

## Acceptance test

This needs a cluster after `make up`, with `postgres-password` in Key Vault. Run from the repo root.

P5-05 (#59): a new migration applies once, and a failure stops the rollout.

1. After the morning bootstrap, check the Job and the version:

   ```bash
   kubectl --context aks-v2-platform -n apps logs job/db-migrate
   kubectl --context aks-v2-platform exec -n data postgres-0 -- \
     psql -U postgres -d orders -tAc "SELECT version, dirty FROM schema_migrations"
   ```

   Expect `1/u baseline` and `1|f`.

2. `argocd app sync migrations`. Expect the Job's log to say `no change`, and the version still `1|f`.
3. On a scratch branch, add `sql/000002_fail.up.sql` containing `SELECT 1/0;` and list it in `kustomization.yaml`. Point the cluster at the branch ([Testing a branch](../../argocd/README.md#testing-a-branch)). Expect `argocd app get migrations` to show the sync operation `Failed`, `2|t` from `schema_migrations`, and in the Job's log `division by zero` if you catch the first run, or `Dirty database version 2` from a later retry.
4. Recover as in [When a migration fails](#when-a-migration-fails), setting the version back to 1. Then apply the argocd root with `main`, or the branch under test, again, and end the root's running operation and any child's still running on the old revision ([Testing a branch](../../argocd/README.md#testing-a-branch)). `migrations` then syncs on `main` and reports `no change`.
