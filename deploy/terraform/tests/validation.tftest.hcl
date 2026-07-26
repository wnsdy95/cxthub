mock_provider "google" {}
mock_provider "vercel" {}

run "reject_firebase_placeholder" {
  command = plan

  variables {
    domain               = "cxthub.com"
    gcp_project          = "cxthub-prod"
    image_tag            = "0123456789abcdef0123456789abcdef01234567"
    firebase_project     = "example-firebase-project"
    firebase_web_api_key = "replace-with-firebase-web-api-key"
  }

  expect_failures = [var.firebase_web_api_key]
}
