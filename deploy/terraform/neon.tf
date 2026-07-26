# Neon credential is not created or read by Terraform. The provider's computed password is not stored in plain text in the tfstate.
# google_secret_manager_secret_version.secret_data remains in plain text in the tfstate.
#
# Bootstrap sequence:
#   1. Create project/database/least-privilege role in Neon.
#   2. Create var.postgres_secret_id in Secret Manager and inject the DSN as a version.
#   3. Terraform manages only the metadata lookup and IAM binding.

data "google_secret_manager_secret" "postgres_dsn" {
  project   = var.gcp_project
  secret_id = var.postgres_secret_id
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
