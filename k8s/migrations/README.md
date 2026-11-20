# migrations

**TL;DR:** Schema changes for the shared `orders` database are numbered SQL files in `sql/`. A Kubernetes Job in `apps` applies the pending ones with [golang-migrate](https://github.com/golang-migrate/migrate), under a Postgres advisory lock, then exits. It connects as `app`, the role that owns the database and that the services use. Postgres starts empty every morning, so `make cluster` runs `make migrate` last, after `make data`, and it fails if the Job fails. P6 rolls out the services after it, and from P7 ArgoCD runs the same Job as a PreSync hook. Changes are expand/contract and roll forward: there are no down migrations. The design is in [Database migrations](../../docs/architecture-decisions.md#database-migrations).

Filled by P5-04 (#58) and P5-05 (#59).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `external-secret.yaml` | `ExternalSecret` | `db-migrate` | Copies Key Vault `postgres-password`, the `app` role's password, into the Secret `db-migrate`, key `PGPASSWORD`. Pods in `apps` can't read `postgres-credentials` in `data` |
| `kustomization.yaml` | `ConfigMap` (generated) | `db-migrate-<hash>` | Every file in `sql/`, mounted into the Job at `/migrations` |
| `job.yaml` | `Job` | `db-migrate-<newest version>` | Runs `migrate up` once and exits |

The SQL ships in a ConfigMap, not an image, so a migration needs no CI build or registry push. A ConfigMap holds up to 1 MiB, far more than this schema needs. The name carries a hash of the SQL, so a changed file always mounts as a new ConfigMap.

## How a run works

1. The Job's pod reads the password from the `db-migrate` Secret as `PGPASSWORD`. The driver, `lib/pq`, reads that variable, so the password never goes into a URL and needs no escaping. An ESO-templated URL would break on a password containing `@`, `:` or `/`.
2. golang-migrate takes a Postgres advisory lock. A second run waits up to 60 seconds (`-lock-timeout=60`), then finds nothing to do.
3. It reads the version from `schema_migrations` and applies each newer file in order. Each file runs as one implicit transaction, so a failing file leaves no partial change.
4. Every statement runs with `lock_timeout=10s`. A DDL statement that can't get its table lock in 10 seconds fails, so it doesn't block the services' queries behind a long transaction. The Job retries twice (`backoffLimit: 2`).
5. The pod exits 0 and the Job completes. Applying the same version again does nothing, since the Job already exists with that name. Once `ttlSecondsAfterFinished` removes it after a day, a new run finds no change.

A run that fails three times, or runs past `activeDeadlineSeconds` (5 minutes), ends `Failed`. `make migrate` then prints the logs and exits 1, so nothing after it runs.

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
2. List it under `configMapGenerator.files` in `kustomization.yaml`. Kustomize has no glob.
3. Bump the Job name in `job.yaml` to the new number: `db-migrate-000002`. If you forget, `make migrate` fails with `field is immutable`, since the Job would mount a different ConfigMap under the same name.
4. Keep the change backward compatible with the running release: see [expand and contract](../../docs/architecture-decisions.md#database-migrations). Write nothing that only the new release can live with.
5. `CREATE INDEX CONCURRENTLY` can't run inside a transaction, so put it in its own file, as the only statement.

Never edit or renumber a file that has been applied anywhere. golang-migrate records only the version number, so an edit to an applied file never runs.

## Run it

Postgres (`make data`, P5-01) must be running first. Data doesn't survive the evening destroy, so every morning Postgres starts empty and this Job rebuilds the schema. `make cluster` runs it last, after `make data`. It needs only the public `migrate/migrate` image, no service image. To run it alone:

```bash
make migrate
```

P6 applies the Deployments after it, so a failed migration stops `make` before any rollout. From P7 ArgoCD runs the Job as a PreSync hook in the same Application as the Deployments, with the ConfigMap and the ExternalSecret as PreSync hooks one wave earlier. The annotations are already on the manifests and kubectl ignores them.

## When a migration fails

golang-migrate marks the version dirty before running it. The file's transaction rolled back, so the schema is unchanged, but every later run stops with `Dirty database version N. Fix and force version.`

1. Read the error: `kubectl --context aks-v2-platform logs -n apps job/db-migrate-<version>`.
2. Fix the file. It never applied, so editing it is safe.
3. Mark the previous version clean. `force` is a plain update of the version table:

   ```bash
   kubectl --context aks-v2-platform exec -n data postgres-0 -- \
     psql -U postgres -d orders -c "UPDATE schema_migrations SET version = <N-1>, dirty = false"
   ```

4. Delete the failed Job, then run `make migrate` again:

   ```bash
   kubectl --context aks-v2-platform delete job -n apps db-migrate-<version>
   make migrate
   ```

The services were never rolled out, so they are still on the release that matches the old schema.

## Validation

Offline, from the repo root:

```bash
kustomize build k8s/migrations | kubeconform -strict -ignore-missing-schemas -summary
```

With Docker, against a throwaway `postgres:16`, running the pinned image as the Job does (user 65532, read-only root, no capabilities, migrations mounted through `..data` symlinks like a ConfigMap volume):

| Check | Result |
| --- | --- |
| Baseline on an empty database | `1/u baseline`, 10 tables, 7 templates |
| Run again | `no change` |
| Two runs at once, one 8-second migration | The second waited for the advisory lock, then `no change` |
| A file whose second statement fails | Nothing from the file applied, version marked dirty, next run refuses until forced |
| An `ALTER TABLE` behind an open transaction | Failed after 10 seconds with `lock timeout` instead of waiting |
| Password with `@`, `:`, `/` and `#` in `PGPASSWORD` only | Connected |
| The five services' own `migrate()` on an empty database, versus the baseline | `pg_dump -s` identical |
| The five services starting on a database the baseline built | All five migrated and started |
| The Job and the five services as a non-superuser `app` that owns the database, as in `k8s/data` | Baseline applied, tables owned by `app`, all five started |
| The services connecting as a role that doesn't own the tables | All five exited: `must be owner of table` |

## Acceptance test

This needs Postgres (P5-01) running in `data`, with `postgres-password` in Key Vault. After `make cluster` the baseline has already run, so step 1 shows the Job's earlier logs. Run from the repo root.

P5-05 (#59): a new migration applies once, and a failure stops the rollout.

1. `make migrate`. Expect `1/u baseline` and exit 0. Then check the version:

   ```bash
   kubectl --context aks-v2-platform exec -n data postgres-0 -- \
     psql -U postgres -d orders -tAc "SELECT version, dirty FROM schema_migrations"
   ```

   Expect `1|f`.

2. `make migrate` again. The Job already exists and is complete, so nothing runs, `make` prints the same logs and the version stays `1|f`.
3. On a scratch branch, add `sql/000002_fail.up.sql` containing `SELECT 1/0;`, list it in `kustomization.yaml` and rename the Job `db-migrate-000002`. `make migrate` should print `division by zero`, then `failed. Do not roll out the apps`, and exit 1.
4. Recover as in [When a migration fails](#when-a-migration-fails), forcing version 1, then drop the scratch branch and delete the Job.
