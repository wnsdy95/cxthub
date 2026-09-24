locals {
  github_app_public = var.github_app == null ? {} : {
    CXT_GITHUB_APP_ID        = var.github_app.id
    CXT_GITHUB_APP_CLIENT_ID = var.github_app.client_id
    CXT_GITHUB_APP_SLUG      = var.github_app.slug
  }
  github_app_secrets = var.github_app == null ? {} : {
    CXT_GITHUB_APP_CLIENT_SECRET  = var.github_app.client_secret_id
    CXT_GITHUB_APP_PRIVATE_KEY    = var.github_app.private_key_secret_id
    CXT_GITHUB_APP_WEBHOOK_SECRET = var.github_app.webhook_secret_id
  }
}
data "google_secret_manager_secret" "github_app" {
  for_each  = local.github_app_secrets
  project   = var.gcp_project
  secret_id = each.value
}
resource "google_secret_manager_secret_iam_member" "cxtd_github_app" {
  for_each  = data.google_secret_manager_secret.github_app
  project   = var.gcp_project
  secret_id = each.value.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.cxtd.email}"
}
