package router

import (
	"fmt"
	"strings"
)

// RouteInfo represents the structured components of an AI Platform / Gemini request URI.
type RouteInfo struct {
	APIVersion   string // e.g. "v1", "v1beta1"
	ProjectID    string // e.g. "my-project-123"
	Region       string // e.g. "us-central1"
	Publisher    string // e.g. "google"
	Model        string // e.g. "gemini-1.5-pro"
	Method       string // e.g. "streamGenerateContent", "generateContent", "countTokens"
	RawQuery     string // e.g. "alt=sse"
	Host         string // e.g. "us-central1-aiplatform.googleapis.com"
	OriginalPath string // Full original raw path (including query)
}

// IsStreaming returns true if the request method is streamGenerateContent.
func (r *RouteInfo) IsStreaming() bool {
	return r.Method == "streamGenerateContent"
}

// OrgKey returns the organization-level quota key formatted as "region/model".
func (r *RouteInfo) OrgKey() string {
	return fmt.Sprintf("%s/%s", strings.ToLower(r.Region), strings.ToLower(r.Model))
}

// ProjectKey returns the project-level quota key formatted as "project/region/model".
func (r *RouteInfo) ProjectKey() string {
	return fmt.Sprintf("%s/%s/%s", strings.ToLower(r.ProjectID), strings.ToLower(r.Region), strings.ToLower(r.Model))
}
