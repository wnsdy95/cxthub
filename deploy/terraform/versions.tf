# cxthub deployment stack — Vercel(web) + Cloud Run(API) + Neon(Postgres).
#
# ⚠ Still does not apply — This code is the blueprint design of the deployment structure (refer to docs/DEPLOYMENT.md).
# backend bucket/prefix must be passed to terraform init -backend-config.

terraform {
  required_version = ">= 1.7"

  required_providers {
    vercel = {
      source  = "vercel/vercel"
      version = "~> 5.4"
    }
    google = {
      source  = "hashicorp/google"
      version = "~> 7.40"
    }
  }

  backend "gcs" {}
}

provider "vercel" {
  # Authenticates using the VERCEL_API_TOKEN environment variable.
}

provider "google" {
  project = var.gcp_project
  region  = var.gcp_region
}
