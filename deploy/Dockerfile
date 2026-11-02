# cxtd container — API deployment (Render or Cloud Run) (multi-stage, postgres tag build).
# build (from repo root): docker build -f deploy/Dockerfile -t <registry>/cxtd:<git-sha> .

FROM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS build
WORKDIR /src
COPY backend/ backend/
RUN cd backend && CGO_ENABLED=0 go build -tags postgres -o /out/cxtd ./cmd/cxtd

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
WORKDIR /app
COPY --from=build /out/cxtd /app/cxtd
COPY --from=build /src/backend/internal/domain/legacy_native_float64/ /usr/share/doc/cxthub/legacy-native-float64/
# Migration SQL bundled for read-only startup verification (CXT_MIGRATIONS_DIR=/app/migrations)
COPY schemas/db/migrations/ /app/migrations/
ENV CXT_MIGRATIONS_DIR=/app/migrations
ENV CXT_REQUIRE_POSTGRES=1
ENV GOMEMLIMIT=400MiB
USER nonroot:nonroot
EXPOSE 8907
# Production required env vars are injected by terraform (cloudrun.tf): CXT_AUTH/CXT_POSTGRES_DSN/…
# Binding to 0.0.0.0 requires both verified auth and PostgreSQL; missing DSN
# fails before the HTTP listener starts (cmd/cxtd/main.go).
ENTRYPOINT ["/app/cxtd", "serve", "--addr", "0.0.0.0:8907"]
