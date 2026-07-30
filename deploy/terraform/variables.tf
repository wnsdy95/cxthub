variable "domain" {
  description = "A single service domain exposed to users (e.g., cxthub.com). /api is proxied by Vercel to Cloud Run."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$", var.domain))
    error_message = "domain must be a lowercase DNS name without a scheme/path (e.g., cxthub.com)."
  }
}

variable "gcp_project" {
  description = "GCP project ID where Cloud Run/Artifact Registry will reside."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.gcp_project))
    error_message = "gcp_project must be in GCP project ID format."
  }
}

variable "gcp_region" {
  description = "Cloud Run region."
  type        = string
  default     = "asia-northeast3" # Seoul

  validation {
    condition     = can(regex("^[a-z]+(?:-[a-z]+)+[0-9]+$", var.gcp_region))
    error_message = "gcp_region must be in valid GCP region name format (e.g., asia-northeast3)."
  }
}

variable "image_tag" {
  description = "cxtd container tag to deploy (git SHA recommended). Apply after CI/Manual build pushes."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{40}$", var.image_tag))
    error_message = "image_tag must be a 40-character git commit SHA that precisely identifies the deployment source."
  }
}

variable "firebase_project" {
  description = "Firebase authentication project ID (CXT_AUTH=firebase)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.firebase_project))
    error_message = "firebase_project must be in Firebase/GCP project ID format."
  }
}

variable "firebase_web_api_key" {
  description = "Firebase Web App API key. Identifier exposed in browser bundles and must apply Firebase API/domain restrictions."
  type        = string

  validation {
    condition = (
      length(trimspace(var.firebase_web_api_key)) >= 20 &&
      !can(regex("\\s", var.firebase_web_api_key)) &&
      !startswith(lower(trimspace(var.firebase_web_api_key)), "replace-with-")
    )
    error_message = "firebase_web_api_key must be a real Firebase Web App API key, not a blank or example placeholder."
  }
}

variable "firebase_auth_domain" {
  description = "Firebase Web Auth domain. null if <firebase_project>.firebaseapp.com."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = var.firebase_auth_domain == null || can(regex(
      "^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$",
      var.firebase_auth_domain
    ))
    error_message = "firebase_auth_domain must be a lowercase DNS name without a scheme/path."
  }
}

variable "postgres_secret_id" {
  description = "Neon DSN pre-injected Secret Manager secret ID. The secret version is managed outside Terraform."
  type        = string
  default     = "cxt-postgres-dsn"

  validation {
    condition     = can(regex("^[A-Za-z0-9_-]{1,255}$", var.postgres_secret_id))
    error_message = "postgres_secret_id must be a Secret Manager ID containing only letters, numbers, hyphens, and underscores."
  }
}

variable "github_webhook_secret_id" {
  description = "GitHub PR webhook HMAC secret ID pre-injected into Secret Manager. The secret version is managed outside Terraform."
  type        = string
  default     = "cxt-github-webhook-secret"

  validation {
    condition     = can(regex("^[A-Za-z0-9_-]{1,255}$", var.github_webhook_secret_id))
    error_message = "github_webhook_secret_id must be a Secret Manager ID containing only letters, numbers, hyphens, and underscores."
  }
}

variable "vercel_team_id" {
  description = "Vercel team ID (team_…). null for individual accounts. Do not include slugs or usernames."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.vercel_team_id == null || can(regex("^team_[A-Za-z0-9]+$", var.vercel_team_id))
    error_message = "vercel_team_id must be a Vercel team ID starting with team_ or null."
  }
}

variable "github_repository" {
  description = "Vercel Git integration target owner/repository."
  type        = string
  default     = "wnsdy95/cxthub"

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must be in owner/repository format."
  }
}
