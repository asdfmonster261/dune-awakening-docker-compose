#!/bin/bash
# Mounted at /docker-entrypoint-initdb.d/01-init.sh inside the postgres container.
# Runs only on first cluster init (when PGDATA is empty).
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
    CREATE ROLE dune WITH LOGIN PASSWORD '${POSTGRES_DUNE_PASS}';
    ALTER ROLE dune CREATEDB;
    GRANT ALL PRIVILEGES ON DATABASE dune TO dune;
EOSQL
