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

// MutateHost generates the appropriate target hostname if the region changes.
// e.g. "us-central1-aiplatform.googleapis.com" -> "us-east4-aiplatform.googleapis.com"
func MutateHost(info *RouteInfo, targetRegion string) string {
	if info == nil {
		return ""
	}
	if targetRegion == "" || targetRegion == info.Region {
		return info.Host
	}

	// If the existing host contains "aiplatform.googleapis.com", replace the region prefix
	if strings.Contains(info.Host, "aiplatform.googleapis.com") {
		return fmt.Sprintf("%s-aiplatform.googleapis.com", targetRegion)
	}

	// Fallback to existing host if custom hostname or private DNS is used without region prefix
	return info.Host
}
