# The image of the one-off job in render-runtime-rights.yaml: psql runs the
# rights script of migrations 0161 to 0170 as CXT_POSTGRES_DSN, the database's
# default account (runtime-rights.sh). The script is the one in this build's
# commit.
FROM postgres:16-alpine@sha256:f2f98726820d0342055351c6e9d36e705f5bfffb3dd53ff556325114b08afb6d
COPY schemas/db/runtime_rights/0161_0170.sql /runtime_rights/0161_0170.sql
COPY deploy/runtime-rights.sh /runtime_rights/run.sh
USER postgres
ENTRYPOINT ["sh", "/runtime_rights/run.sh"]
