data "google_compute_network" "vpc" {
  name    = var.network_name
  project = var.project_id
}

# Private DNS zone intercepting aiplatform.googleapis.com and regional
# *-aiplatform.googleapis.com traffic inside the VPC and routing it to the
# Internal Application Load Balancer VIP.
resource "google_dns_managed_zone" "aiplatform_private" {
  name        = "aiplatform-googleapis-private"
  dns_name    = "aiplatform.googleapis.com."
  description = "Private DNS zone routing Gemini API traffic through the Priority Quota Governor ILB"
  visibility  = "private"

  private_visibility_config {
    networks {
      network_url = data.google_compute_network.vpc.id
    }
  }
}

resource "google_dns_record_set" "aiplatform_apex" {
  name         = google_dns_managed_zone.aiplatform_private.dns_name
  managed_zone = google_dns_managed_zone.aiplatform_private.name
  type         = "A"
  ttl          = 60
  rrdatas      = [google_compute_address.governor_ilb_vip.address]
}

resource "google_dns_record_set" "aiplatform_wildcard" {
  name         = "*.${google_dns_managed_zone.aiplatform_private.dns_name}"
  managed_zone = google_dns_managed_zone.aiplatform_private.name
  type         = "A"
  ttl          = 60
  rrdatas      = [google_compute_address.governor_ilb_vip.address]
}
