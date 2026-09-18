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

run "durable_workers_keep_cpu_without_requests" {
  command = plan

  variables {
    domain               = "cxthub.com"
    gcp_project          = "cxthub-prod"
    image_tag            = "0123456789abcdef0123456789abcdef01234567"
    firebase_project     = "example-firebase-project"
    firebase_web_api_key = "AIzaSyExampleFirebaseWebApiKey123456"
  }

  assert {
    condition     = google_cloud_run_v2_service.cxtd.template[0].scaling[0].min_instance_count >= 1 && google_cloud_run_v2_service.cxtd.template[0].containers[0].resources[0].cpu_idle == false
    error_message = "Durable delivery requires at least one instance with CPU available outside HTTP requests."
  }
}
