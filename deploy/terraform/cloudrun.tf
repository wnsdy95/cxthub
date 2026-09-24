# Cloud Run — cxtd(API daemon). Containers are built using deploy/Dockerfile and pushed to Artifact Registry.
#
# Pairing, OAuth, and request allowances use shared PostgreSQL state.
# Raise the default cap only after staging load and database pool validation.

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
      min_instance_count = 1 # Keep one warm instance.
      max_instance_count = var.max_instances
    }

    containers {
      image = local.image

      # Durable PR/notification queues must keep running without HTTP traffic.
      # Instance-based billing charges the full lifetime of the warm instance.
      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        cpu_idle = false
      }

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
        name  = "CXT_PUBLIC_URL"
        value = "https://${var.domain}"
      }
      env {
        name  = "CXT_REQUIRE_POSTGRES"
        value = "1"
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

      dynamic "env" {
        for_each = local.github_app_public
        content {
          name  = env.key
          value = env.value
        }
      }
      dynamic "env" {
        for_each = data.google_secret_manager_secret.github_app
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = env.value.secret_id
              version = "latest"
            }
          }
        }
      }

      dynamic "env" {
        for_each = var.resend_secret_id == "" ? [] : [var.resend_secret_id]
        content {
          name = "RESEND_API_KEY"
          value_source {
            secret_key_ref {
              secret  = data.google_secret_manager_secret.resend[0].secret_id
              version = "latest"
            }
          }
        }
      }
      env {
        name  = "RESEND_FROM"
        value = var.resend_from
      }
      env {
        name  = "CXT_WEB_URL"
        value = "https://${var.domain}"
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
    google_secret_manager_secret_iam_member.cxtd_resend,
    google_secret_manager_secret_iam_member.cxtd_github_app,
  ]
}

# Allow unauthenticated public calls (authentication is handled by the app layer — session/token/role gates are responsible).
resource "google_cloud_run_v2_service_iam_member" "public" {
  name     = google_cloud_run_v2_service.cxtd.name
  location = var.gcp_region
  role     = "roles/run.invoker"
  member   = "allUsers"
}
