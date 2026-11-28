#!/bin/sh
# The job of render-runtime-rights.yaml: psql runs the rights script of
# migrations 0161 to 0170 as the account in CXT_POSTGRES_DSN. Where PGPASSWORD
# is set, it is that account's password, and the one in the address is left
# out: the address Render keeps can hold a password changed since.
set -eu
dsn=$CXT_POSTGRES_DSN
if [ -n "${PGPASSWORD:-}" ]; then
  dsn=$(printf '%s' "$dsn" | sed -E 's#^([a-z]+://[^:/@]+):[^@]*@#\1@#')
fi
exec psql "$dsn" -v ON_ERROR_STOP=1 -f /runtime_rights/0161_0170.sql
