# migrations

**TL;DR:** Schema changes for the shared `orders` database are numbered SQL files in `sql/`. A Kubernetes Job in `apps` applies the pending ones with [golang-migrate](https://github.com/golang-migrate/migrate), under a Postgres advisory lock, then exits. It connects as `app`, the role that owns the database and that the services use. Postgres starts empty every morning, so `make cluster` runs `make migrate` last, after `make data`, and it fails if the Job fails. P6 rolls out the services after it, and from P7 ArgoCD runs the same Job as a PreSync hook. Changes are expand/contract and roll forward: there are no down migrations. The design is in [Database migrations](../../docs/architecture-decisions.md#database-migrations).

Filled by P5-04 (#58) and P5-05 (#59).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `external-secret.yaml` | `ExternalSecret` | `db-migrate` | Copies Key Vault `postgres-password`, the `app` role's password, into the Secret `db-migrate`, key `PGPASSWORD`. Pods in `apps` can't read `postgres-credentials` in `data`. Syncs once, then only on `force-sync`, like the services' copies ([data](../data/README.md#rotating-a-password)) |
| `kustomization.yaml` | `ConfigMap` (generated) | `db-migrate` | Every file in `sql/`, mounted into the Job at `/migrations` |
| `job.yaml` | `Job` | `db-migrate` | Runs `migrate up` once and exits |
| `networkpolicy.yaml` | `NetworkPolicy` | `db-migrate` | Out to Postgres on 5432 only. `apps` denies everything else ([namespaces](../namespaces/README.md#networkpolicies)). A PreSync hook in P7, a wave before the Job, so it exists before the Job's pod starts |

The SQL ships in a ConfigMap, not an image, so a migration needs no CI build or registry push. A ConfigMap holds up to 1 MiB, far more than this schema needs. The ConfigMap's name has no hash suffix. The Job is recreated on every run, so it always mounts the current SQL. From P7, hashed names would pile up, since ArgoCD never prunes hook resources.

## How a run works

1. `make migrate` checks that the Postgres StatefulSet is ready, deletes any earlier `db-migrate` Job, applies this folder and waits for the new Job. Deleting first matters: a finished Job of the same name would leave the apply unchanged, and `make` would report success without migrating, for example after the database was reset.
2. The Job's pod reads the password from the `db-migrate` Secret as `PGPASSWORD`. The driver, `lib/pq`, reads that variable, so the password never goes into a URL and needs no escaping. An ESO-templated URL would break on a password containing `@`, `:` or `/`.
3. golang-migrate takes a Postgres advisory lock. A second run waits up to 60 seconds (`-lock-timeout=60`) for the first to finish, then finds nothing to do.
4. It reads the version from `schema_migrations` and applies each newer file in order. Each file runs as one implicit transaction, so a failing file leaves no partial change.
5. Each file starts with `SET lock_timeout = '10s';`. A DDL statement that can't get its table lock in 10 seconds fails, so it doesn't block the services' queries behind a long transaction. The setting lives in the files, not the connection URL: in the URL it would also cut golang-migrate's wait for the advisory lock to 10 seconds.
6. The pod exits 0 and the Job completes. A second run with nothing new reports `no change`.

`backoffLimit: 2` retries failures before any file runs: Postgres restarting, or the advisory lock not freed within 60 seconds. It can't recover a failure inside a file. golang-migrate marks the version dirty before running a file, so every retry stops at `Dirty database version N` (see [When a migration fails](#when-a-migration-fails)). A Job that fails three times, or runs past `activeDeadlineSeconds` (5 minutes), ends `Failed`. `make migrate` then prints the logs and exits 1, so nothing after it runs.

Run one `make migrate` at a time. A second operator's run deletes the first one's Job. If that kills a migration mid-file, Postgres rolls back the file but the version stays dirty.

## What the services do today

The five services that own tables (order, inventory, payment, notification and shipping) create them at startup in `migrate()`, with `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS`. notification-service also seeds seven templates with `ON CONFLICT DO NOTHING`. scheduler and dashboard-api read those tables but create nothing.

`sql/000001_baseline.up.sql` is all five `migrate()` functions copied verbatim. Run before or after the services start, it gives the same schema: `pg_dump -s` of the two orders is identical (see [Validation](#validation)). So both can run safely:

- On an empty database, whichever runs first creates the tables and the other skips them.
- On a database the services already built, the baseline is a no-op and records version 1.

Two rules keep this safe until the services' `migrate()` functions are removed:

- **Never edit `migrate()` in a service again.** Put every schema change in a new file here, or the two sources drift.
- **Run the Job as the role the services connect as, `app`.** `CREATE INDEX IF NOT EXISTS` needs table ownership even when the index exists. A service connecting as a role that doesn't own the tables exits at startup with `must be owner of table orders`. Tables the superuser created would belong to `postgres`, so the Job never uses it. The role is set in `DATABASE_URL` in `job.yaml`.

Removing `migrate()` from the services is a follow-up once this Job runs before every rollout. It also stops each pod start taking a brief `SHARE` lock on every table, which `CREATE INDEX IF NOT EXISTS` takes even when the index exists.

## Add a migration

1. Add `sql/<next number>_<what>.up.sql`, six digits, one more than the last: `000002_orders_add_gift_message.up.sql`.
2. Start it with `SET lock_timeout = '10s';`.
3. List it under `configMapGenerator.files` in `kustomization.yaml`. Kustomize has no glob.
4. Keep the change backward compatible with the running release: see [expand and contract](../../docs/architecture-decisions.md#database-migrations). Write nothing that only the new release can live with.
5. `CREATE INDEX CONCURRENTLY` can't run inside a transaction, so put it in its own file, as the only statement, without the `SET` line.

Never edit or renumber a file that has been applied anywhere. golang-migrate records only the version number, so an edit to an applied file never runs.

## Run it

Postgres (`make data`, P5-01) must be running first. Data doesn't survive the evening destroy, so every morning Postgres starts empty and this Job rebuilds the schema. `make cluster` runs it last, after `make data`. It needs only the public `migrate/migrate` image, no service image. To run it alone:

```bash
make migrate
```

P6 applies the Deployments after it, so a failed migration stops `make` before any rollout. From P7 ArgoCD runs the Job as a PreSync hook in the same Application as the Deployments, with the ConfigMap and the ExternalSecret as PreSync hooks one wave earlier. The annotations are already on the manifests and kubectl ignores them.

## When a migration fails

golang-migrate marks the version dirty before running a file. The file's transaction rolled back, so the schema is unchanged, but every later run stops with `Dirty database version N. Fix and force version.`

1. Read the error: `kubectl --context aks-v2-platform logs -n apps job/db-migrate`.
2. Fix the file. It never applied, so editing it is safe.
3. Clear the dirty version. What to run depends on which file failed:

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

4. Run `make migrate` again. It replaces the failed Job.

The services were never rolled out, so they are still on the release that matches the old schema.

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

The `make migrate` recipe was run under GNU Make 3.81 with a stub in place of kubectl. It stopped when Postgres wasn't ready, survived a failed `kubectl get`, still exited 0 when the logs couldn't be read, and exited 1 on a failed Job.

## Acceptance test

This needs Postgres (P5-01) running in `data`, with `postgres-password` in Key Vault. Run from the repo root.

P5-05 (#59): a new migration applies once, and a failure stops the rollout.

1. `make migrate`. On a fresh morning database expect `1/u baseline`, or `no change` if `make cluster` already ran it, and exit 0. Then check the version:

   ```bash
   kubectl --context aks-v2-platform exec -n data postgres-0 -- \
     psql -U postgres -d orders -tAc "SELECT version, dirty FROM schema_migrations"
   ```

   Expect `1|f`.

2. `make migrate` again. Expect `no change` and the version still `1|f`.
3. On a scratch branch, add `sql/000002_fail.up.sql` containing `SELECT 1/0;` and list it in `kustomization.yaml`. `make migrate` should print `division by zero`, then `failed. Do not roll out the apps`, and exit 1.
4. Recover as in [When a migration fails](#when-a-migration-fails), setting the version back to 1. Then switch back from the scratch branch and run `make migrate`, which reports `no change`.
