#!/bin/bash
# Creates the app role the seven services connect as and hands it the orders
# database, so migrations can create tables. The image runs this once, from
# /docker-entrypoint-initdb.d, when PGDATA is empty. psql reads the password
# from the environment with \getenv, so it never appears on a command line.
# psql's exit code is the script's, so a failure stops the entrypoint.

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'EOSQL'
\getenv app_password POSTGRES_PASSWORD
CREATE ROLE app LOGIN PASSWORD :'app_password';
ALTER DATABASE orders OWNER TO app;
EOSQL
