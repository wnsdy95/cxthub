# cxtd container — Cloud Run deployment (multi-stage, postgres tag build).
# build (from repo root): docker build -f deploy/Dockerfile -t <registry>/cxtd:<git-sha> .

FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY backend/ backend/
RUN cd backend && CGO_ENABLED=0 go build -tags postgres -o /out/cxtd ./cmd/cxtd

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
WORKDIR /app
COPY --from=build /out/cxtd /app/cxtd
# Migration SQL bundled — automatically applied on startup (CXT_MIGRATIONS_DIR=/app/migrations)
COPY schemas/db/migrations/ /app/migrations/
USER nonroot:nonroot
EXPOSE 8907
# Production required env vars are injected by terraform (cloudrun.tf): CXT_AUTH/CXT_POSTGRES_DSN/…
# Binding to 0.0.0.0 is only allowed with CXT_AUTH=firebase (dev auth guard — cmd/cxtd/main.go)
ENTRYPOINT ["/app/cxtd", "serve", "--addr", "0.0.0.0:8907"]
