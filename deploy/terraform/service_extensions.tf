resource "google_network_services_lb_traffic_extension" "governor_extproc_callout" {
  count = var.enable_service_extensions ? 1 : 0

  name                  = "governor-extproc-callout"
  description           = "Calls out from the Regional Internal ALB to the Priority Quota Governor ext_proc service"
  location              = var.region
  load_balancing_scheme = "INTERNAL_MANAGED"
  forwarding_rules      = [google_compute_forwarding_rule.governor_ilb_forwarding_rule.id]

  extension_chains {
    name = "governor-priority-chain"

    match_condition {
      cel_expression = "true"
    }

    extensions {
      name      = "governor-extproc"
      authority = "governor.internal"
      service   = google_compute_region_backend_service.governor_dataplane_backend.id
      timeout   = "0.2s"
      fail_open = true

      supported_events = ["REQUEST_HEADERS"]
    }
  }
}
