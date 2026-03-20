# SRE Companion Agent — Standalone PoC Deployment
#
# Usage:
#   terraform init
#   terraform apply -var="project_id=my-dev-project"

terraform {
  required_version = ">= 1.5"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = ">= 5.0"
    }
  }
}

variable "project_id" {
  description = "GCP project ID to deploy into"
  type        = string
}

variable "region" {
  description = "GCP region for Cloud Run and Workflows"
  type        = string
  default     = "us-central1"
}

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

locals {
  ar_image = "${var.region}-docker.pkg.dev/${var.project_id}/quay-io/patmarti/sre-companion-agent:latest"
}

# --- APIs ---

resource "google_project_service" "apis" {
  for_each = toset([
    "run.googleapis.com",
    "workflows.googleapis.com",
    "workflowexecutions.googleapis.com",
    "aiplatform.googleapis.com",
    "artifactregistry.googleapis.com",
    "generativelanguage.googleapis.com",
  ])

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# --- Artifact Registry: quay.io pull-through ---

resource "google_artifact_registry_repository" "quay" {
  project       = var.project_id
  location      = var.region
  repository_id = "quay-io"
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"

  remote_repository_config {
    docker_repository {
      custom_repository {
        uri = "https://quay.io"
      }
    }
  }

  depends_on = [google_project_service.apis]
}

# --- Service Agents ---

# Provision the Vertex AI service agent
resource "google_project_service_identity" "aiplatform" {
  provider = google-beta
  project  = var.project_id
  service  = "aiplatform.googleapis.com"

  depends_on = [google_project_service.apis]
}

# --- Service Account ---

resource "google_service_account" "agent" {
  project      = var.project_id
  account_id   = "sre-companion-agent"
  display_name = "SRE Companion Agent"

  depends_on = [google_project_service.apis]
}

resource "google_project_iam_member" "agent" {
  for_each = toset([
    "roles/container.developer",
    "roles/workflows.invoker",
    "roles/workflows.viewer",
    "roles/aiplatform.user",
    "roles/logging.viewer",
    "roles/compute.viewer",
  ])

  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.agent.email}"
}

# --- Cloud Run Service ---

resource "google_cloud_run_v2_service" "agent" {
  name     = "sre-companion-agent"
  location = var.region
  project  = var.project_id
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.agent.email

    scaling {
      min_instance_count = 0
      max_instance_count = 2
    }

    containers {
      image = local.ar_image

      env {
        name  = "GCP_PROJECT_ID"
        value = var.project_id
      }
      env {
        name  = "GCP_REGION"
        value = var.region
      }
      env {
        name  = "VERTEX_AI_MODEL"
        value = "gemini-2.5-flash"
      }

      startup_probe {
        http_get {
          path = "/health"
          port = 8080
        }
      }

      liveness_probe {
        http_get {
          path = "/health"
          port = 8080
        }
      }
    }
  }

  depends_on = [
    google_project_service.apis,
    google_project_iam_member.agent,
    google_artifact_registry_repository.quay,
    google_project_service_identity.aiplatform,
  ]
}

# --- Access: gcp-hcp-eng group ---

resource "google_cloud_run_v2_service_iam_member" "invoker" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.agent.name
  role     = "roles/run.invoker"
  member   = "group:gcp-hcp-eng@redhat.com"
}

# --- Outputs ---

output "service_url" {
  value = google_cloud_run_v2_service.agent.uri
}

output "image" {
  value = local.ar_image
}
