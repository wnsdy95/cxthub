# Cloud Run — cxtd(API daemon). Containers are built using deploy/Dockerfile and pushed to Artifact Registry.
#
# ⚠ Keep exactly one instance (min=max=1): device-flow pairing and rate limits are in memory,
#   so polling can hit the wrong instance when scaled out. Before increasing this limit,
#   move pairing state into the store as described in the deployment configuration under Scaling path.

resource "google_artifact_registry_repository" "cxthub" {
  repository_id = "cxthub"
  format        = "DOCKER"
  location      = var.gcp_region

  lifecycle {
    prevent_destroy = true
  }
}

locals {
  image = "${var.gcp_region}-docker.pkg.dev/${var.gcp_project}/cxthub/cxtd:${var.image_tag}"
}

resource "google_cloud_run_v2_service" "cxtd" {
  name                = "cxtd"
  location            = var.gcp_region
  ingress             = "INGRESS_TRAFFIC_ALL"
  deletion_protection = true

  template {
    service_account = google_service_account.cxtd.email

    scaling {
      min_instance_count = 1 # Avoid cold starts for the in-memory pairing flow.
      max_instance_count = 1 # Externalize pairing state before scaling out.
    }

    containers {
      image = local.image

      # Security settings (see the deployment configuration, Environment variables): require Firebase
      # authentication, secure cookies, and one exact user-facing origin for CSRF checks.
      env {
        name  = "CXT_AUTH"
        value = "firebase"
      }
      env {
        name  = "CXT_FIREBASE_PROJECT"
        value = var.firebase_project
      }
      env {
        name  = "CXT_COOKIE_SECURE"
        value = "1"
      }
      # The browser Origin remains the service domain behind Vercel's external rewrite. It can
      # differ from the upstream run.app Host, so explicitly trust the production domain.
      env {
        name  = "CXT_CORS_ORIGINS"
        value = "https://${var.domain}"
      }
      env {
        name = "CXT_POSTGRES_DSN"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.postgres_dsn.secret_id
            version = "latest"
          }
        }
      }
      env {
        name = "CXT_GITHUB_WEBHOOK_SECRET"
        value_source {
          secret_key_ref {
            secret  = data.google_secret_manager_secret.github_webhook.secret_id
            version = "latest"
          }
        }
      }
      env {
        name  = "CXT_MIGRATIONS_DIR"
        value = "/app/migrations"
      }

      ports {
        container_port = 8907
      }

      # Do not mark a revision ready until the server has connected to the database, applied
      # migrations, and started accepting HTTP. Liveness uses storage-independent process health.
      startup_probe {
        initial_delay_seconds = 0
        timeout_seconds       = 2
        period_seconds        = 2
        failure_threshold     = 60
        http_get {
          path = "/api/v1/health"
          port = 8907
        }
      }

      liveness_probe {
        initial_delay_seconds = 10
        timeout_seconds       = 2
        period_seconds        = 10
        failure_threshold     = 3
        http_get {
          path = "/api/v1/health"
          port = 8907
        }
      }
    }
  }

  depends_on = [
    google_secret_manager_secret_iam_member.cxtd_postgres,
    google_secret_manager_secret_iam_member.cxtd_github_webhook,
  ]
}

# Allow unauthenticated public calls (authentication is handled by the app layer — session/token/role gates are responsible).
resource "google_cloud_run_v2_service_iam_member" "public" {
  name     = google_cloud_run_v2_service.cxtd.name
  location = var.gcp_region
  role     = "roles/run.invoker"
  member   = "allUsers"
}
