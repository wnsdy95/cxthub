# Vercel hosts the SPA and rewrites /api/* to Cloud Run's default run.app URI.
# This provides a same-origin proxy; see frontend/web/vercel.mjs for the cookie/CORS rationale.
# Cloud Run custom-domain mapping is not supported in the Seoul region and is in preview, so it is not used.

resource "vercel_project" "web" {
  name      = "cxthub-web"
  team_id   = var.vercel_team_id
  framework = "vite"

  root_directory = "frontend/web"

  git_repository = {
    type = "github"
    repo = var.github_repository
  }

  # vercel.mjs generates a rewrite to this public origin at build time. Cloud Run must exist
  # first, so the initial Git deployment fails closed instead of using a placeholder origin.
  environment = [
    {
      key       = "CXT_API_ORIGIN"
      value     = google_cloud_run_v2_service.cxtd.uri
      target    = ["production"]
      sensitive = true
    },
    {
      key       = "VITE_FIREBASE_API_KEY"
      value     = var.firebase_web_api_key
      target    = ["production"]
      sensitive = true # Public identifier, marked sensitive for consistent production policy.
    },
    {
      key       = "VITE_FIREBASE_AUTH_DOMAIN"
      value     = var.firebase_auth_domain != null ? var.firebase_auth_domain : "${var.firebase_project}.firebaseapp.com"
      target    = ["production"]
      sensitive = true
    },
    {
      key       = "VITE_FIREBASE_PROJECT_ID"
      value     = var.firebase_project
      target    = ["production"]
      sensitive = true
    },
  ]

  lifecycle {
    prevent_destroy = true
  }
}

# The SPA uses same-origin relative paths (/api/v1), so production leaves VITE_API_BASE unset.
# Set it only when splitting origins; see docs/DEPLOYMENT.md, Option B.

resource "vercel_project_domain" "web" {
  project_id = vercel_project.web.id
  team_id    = var.vercel_team_id
  domain     = var.domain
}
