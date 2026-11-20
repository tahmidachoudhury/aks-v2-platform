-- The app role the seven services connect as, owner of the orders database
-- so migrations can create tables. Safe to run any number of times.
--
-- Two callers: the image runs it once from /docker-entrypoint-initdb.d when
-- PGDATA is empty, and make data runs it after every rollout, which repairs
-- a failed first init and applies a rotated password. The password is read
-- from the mounted Secret file, which follows the Secret, so it never
-- appears on a command line. The superuser gets the same value, the only
-- Postgres secret in the vault.
\set ON_ERROR_STOP on
\set app_password `cat /etc/postgres-credentials/password`
SELECT 'CREATE ROLE app LOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app') \gexec
ALTER ROLE app WITH LOGIN PASSWORD :'app_password';
ALTER ROLE postgres WITH PASSWORD :'app_password';
ALTER DATABASE orders OWNER TO app;
