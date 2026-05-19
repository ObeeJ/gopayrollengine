-- config/postgres-init.sql
-- Runs once on first container boot via postgres:15-alpine's docker-entrypoint
-- initdb mechanism. Creates the application role under which the API and
-- worker connect — NOSUPERUSER NOBYPASSRLS so RLS policies (migration 000008)
-- actually take effect. The superuser account remains available for migrations
-- and operator access but is never used by the running application.
--
-- The role's password is read from $POSTGRES_APP_PASSWORD via psql's `\set`
-- (which captures the host env var) and inlined into the CREATE ROLE statement
-- using the :'name' substitution form (psql escapes it as a quoted literal).
-- Earlier versions of this file used current_setting('app_password') which
-- silently returns NULL because that reads a Postgres GUC, not a psql variable
-- — the role was never created and the application could not authenticate.

\set app_password `printf '%s' "$POSTGRES_APP_PASSWORD"`

-- Loud failure if the env var is empty — better than silently no-op'ing and
-- leaving the application unable to log in.
\if :{?app_password}
\else
    \echo 'FATAL: POSTGRES_APP_PASSWORD is not set; refusing to create payroll_app without a password.'
    \quit 1
\endif

CREATE ROLE payroll_app NOSUPERUSER NOBYPASSRLS LOGIN PASSWORD :'app_password';

-- Grant CRUD on all current and future tables in public so the app can
-- function without being a superuser. The RLS policies remain in force
-- because payroll_app lacks BYPASSRLS.
GRANT USAGE ON SCHEMA public TO payroll_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO payroll_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO payroll_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO payroll_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO payroll_app;
