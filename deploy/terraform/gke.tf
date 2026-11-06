resource "google_container_cluster" "governor_autopilot" {
  count = var.enable_gke_autopilot ? 1 : 0

  name             = "governor-autopilot"
  location         = var.region
  enable_autopilot = true
  network          = data.google_compute_network.vpc.id
  subnetwork       = data.google_compute_subnetwork.ilb_subnet.id

  release_channel {
    channel = "REGULAR"
  }
}
