# Runtime credentials are not created or read by Terraform.
# google_secret_manager_secret_version.secret_data would remain in plain text in the tfstate.
#
# Bootstrap sequence:
#   1. Create project/database/least-privilege role in Neon.
#   2. Create var.postgres_secret_id and var.github_webhook_secret_id in Secret Manager,
#      then inject the DSN and webhook HMAC value as enabled versions.
#   3. Terraform manages only the metadata lookup and IAM binding.

data "google_secret_manager_secret" "postgres_dsn" {
  project   = var.gcp_project
  secret_id = var.postgres_secret_id
}

data "google_secret_manager_secret" "github_webhook" {
  project   = var.gcp_project
  secret_id = var.github_webhook_secret_id
}

resource "google_service_account" "cxtd" {
  account_id   = "cxthub-cxtd"
  display_name = "cxthub Cloud Run runtime"
}

resource "google_secret_manager_secret_iam_member" "cxtd_postgres" {
  project   = var.gcp_project
  secret_id = data.google_secret_manager_secret.postgres_dsn.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.cxtd.email}"
}

resource "google_secret_manager_secret_iam_member" "cxtd_github_webhook" {
  project   = var.gcp_project
  secret_id = data.google_secret_manager_secret.github_webhook.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.cxtd.email}"
}
