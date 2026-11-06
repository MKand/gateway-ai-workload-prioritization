resource "google_service_account" "governor_controlplane_sa" {
  account_id   = "governor-controlplane"
  display_name = "Gemini Quota Governor Control Plane"
}

resource "google_project_iam_member" "controlplane_quotas_viewer" {
  project = var.project_id
  role    = "roles/cloudquotas.viewer"
  member  = "serviceAccount:${google_service_account.governor_controlplane_sa.email}"
}

resource "google_project_iam_member" "controlplane_monitoring_viewer" {
  project = var.project_id
  role    = "roles/monitoring.viewer"
  member  = "serviceAccount:${google_service_account.governor_controlplane_sa.email}"
}

resource "google_service_account" "governor_dataplane_sa" {
  account_id   = "governor-dataplane"
  display_name = "Gemini Quota Governor Data Plane (Envoy ext_proc)"
}

resource "google_cloud_run_v2_service" "governor_controlplane" {
  name     = "governor-controlplane"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_INTERNAL_ONLY"

  template {
    service_account = google_service_account.governor_controlplane_sa.email

    containers {
      image = var.controlplane_image
      ports {
        name           = "h2c"
        container_port = 50051
      }
    }
  }
}

resource "google_cloud_run_v2_service" "governor_dataplane" {
  name     = "governor-dataplane"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"

  template {
    service_account = google_service_account.governor_dataplane_sa.email

    containers {
      image = var.dataplane_image
      ports {
        name           = "h2c"
        container_port = 50052
      }
      env {
        name  = "CONTROL_PLANE_URI"
        value = google_cloud_run_v2_service.governor_controlplane.uri
      }
    }
  }
}

resource "google_compute_region_network_endpoint_group" "dataplane_serverless_neg" {
  name                  = "governor-dataplane-neg"
  network_endpoint_type = "SERVERLESS"
  region                = var.region

  cloud_run {
    service = google_cloud_run_v2_service.governor_dataplane.name
  }
}
