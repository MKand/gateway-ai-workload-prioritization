data "google_compute_subnetwork" "ilb_subnet" {
  name    = var.subnetwork_name
  region  = var.region
  project = var.project_id
}

resource "google_compute_address" "governor_ilb_vip" {
  name         = "governor-ilb-vip"
  region       = var.region
  subnetwork   = data.google_compute_subnetwork.ilb_subnet.id
  address_type = "INTERNAL"
  purpose      = "GCE_ENDPOINT"
}

resource "google_compute_region_ssl_certificate" "aiplatform_wildcard_cert" {
  name_prefix = "governor-aiplatform-cert-"
  region      = var.region
  certificate = file(var.tls_certificate_pem_path)
  private_key = file(var.tls_private_key_pem_path)

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_compute_region_backend_service" "governor_dataplane_backend" {
  name                  = "governor-dataplane-extproc"
  region                = var.region
  protocol              = "HTTP2"
  load_balancing_scheme = "INTERNAL_MANAGED"
  timeout_sec           = 30

  backend {
    group           = google_compute_region_network_endpoint_group.dataplane_serverless_neg.id
    capacity_scaler = 1.0
  }
}

resource "google_compute_region_url_map" "governor_ilb_url_map" {
  name            = "governor-ilb-url-map"
  region          = var.region
  default_service = google_compute_region_backend_service.governor_dataplane_backend.id
}

resource "google_compute_region_target_https_proxy" "governor_ilb_https_proxy" {
  name             = "governor-ilb-https-proxy"
  region           = var.region
  url_map          = google_compute_region_url_map.governor_ilb_url_map.id
  ssl_certificates = [google_compute_region_ssl_certificate.aiplatform_wildcard_cert.id]
}

resource "google_compute_forwarding_rule" "governor_ilb_forwarding_rule" {
  name                  = "governor-ilb-https-fr"
  region                = var.region
  ip_protocol           = "TCP"
  load_balancing_scheme = "INTERNAL_MANAGED"
  port_range            = "443"
  target                = google_compute_region_target_https_proxy.governor_ilb_https_proxy.id
  network               = data.google_compute_network.vpc.id
  subnetwork            = data.google_compute_subnetwork.ilb_subnet.id
  ip_address            = google_compute_address.governor_ilb_vip.id
}

output "ilb_vip_address" {
  description = "Internal Application Load Balancer VIP mapped to *.aiplatform.googleapis.com."
  value       = google_compute_address.governor_ilb_vip.address
}
