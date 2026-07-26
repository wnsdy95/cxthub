output "web_url" {
  value = "https://${var.domain}"
}

output "api_origin" {
  description = "Cloud Run run.app address injected into Vercel CXT_API_ORIGIN (do not use directly in the browser)."
  value       = google_cloud_run_v2_service.cxtd.uri
}

output "postgres_secret" {
  description = "Secret Manager secret ID referenced by Cloud Run (value is not output)."
  value       = data.google_secret_manager_secret.postgres_dsn.secret_id
}

output "dns_records" {
  description = "Instructions for the record to be entered at the domain registrar. The exact value for the project is determined by `vercel domains inspect`."
  value = {
    web = "A ${var.domain} → Recommended Vercel project domain value (default value 76.76.21.21)"
  }
}

output "cli_remote_example" {
  description = "Remote URL format for team onboarding."
  value       = "cxt remote add origin https://${var.domain}/<username>/<workspace>/<repo>"
}
