package router

import (
	"fmt"
	"strings"
)

// Mutate rewrites both the path and the host based on target model and target region overrides.
// If targetModel is empty, the original model is preserved.
// If targetRegion is empty, the original region is preserved.
func Mutate(info *RouteInfo, targetModel, targetRegion string) (newPath string, newHost string) {
	newPath = MutatePath(info, targetModel, targetRegion)
	newHost = MutateHost(info, targetRegion)
	return newPath, newHost
}

// MutatePath generates a new URI path with overridden model and/or region while preserving query strings and method.
func MutatePath(info *RouteInfo, targetModel, targetRegion string) string {
	if info == nil {
		return ""
	}

	model := info.Model
	if targetModel != "" {
		model = targetModel
	}

	region := info.Region
	if targetRegion != "" {
		region = targetRegion
	}

	path := fmt.Sprintf("/%s/projects/%s/locations/%s/publishers/%s/models/%s:%s",
		info.APIVersion,
		info.ProjectID,
		region,
		info.Publisher,
		model,
		info.Method,
	)

	if info.RawQuery != "" {
		path = path + "?" + info.RawQuery
	}

	return path
}

// GlobalLocation is the Vertex AI location served by the global endpoint.
const GlobalLocation = "global"

// vertexDomain is the suffix shared by every Vertex AI endpoint.
const vertexDomain = "aiplatform.googleapis.com"

// HostFor returns the public Vertex AI hostname for a location: the global
// endpoint "aiplatform.googleapis.com" for "global", otherwise
// "{region}-aiplatform.googleapis.com".
func HostFor(location string) string {
	if strings.EqualFold(location, GlobalLocation) {
		return vertexDomain
	}
	return strings.ToLower(location) + "-" + vertexDomain
}

// isVertexHost reports whether host is a public Vertex AI endpoint (global or
// regional), ignoring any port.
func isVertexHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.LastIndexByte(h, ':'); i != -1 {
		h = h[:i]
	}
	return h == vertexDomain || strings.HasSuffix(h, "-"+vertexDomain)
}

// MutateHost returns the hostname to use after a region change, for example
// "us-central1-aiplatform.googleapis.com" -> "europe-west4-aiplatform.googleapis.com"
// or -> "aiplatform.googleapis.com" for the global location. Custom hostnames
// (private DNS, PSC) are kept unchanged.
func MutateHost(info *RouteInfo, targetRegion string) string {
	if info == nil {
		return ""
	}
	if targetRegion == "" || strings.EqualFold(targetRegion, info.Region) {
		return info.Host
	}
	if isVertexHost(info.Host) {
		return HostFor(targetRegion)
	}
	return info.Host
}
