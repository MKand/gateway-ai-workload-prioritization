package router

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrEmptyPath is returned when the provided path is empty.
	ErrEmptyPath = errors.New("path cannot be empty")

	// ErrNotAIPlatformPath is returned when the path does not match AI Platform URL structures.
	ErrNotAIPlatformPath = errors.New("not a recognized AI Platform path")

	// ErrMissingModel is returned when the model segment is missing.
	ErrMissingModel = errors.New("missing model in path")

	// ErrMissingMethod is returned when the RPC method delimiter ':' is missing or method name is empty.
	ErrMissingMethod = errors.New("missing method delimiter ':' or method name in model path")

	// ErrMissingProject is returned when the project segment is missing.
	ErrMissingProject = errors.New("missing project in path")

	// ErrMissingRegion is returned when the location/region segment is missing.
	ErrMissingRegion = errors.New("missing location/region in path")
)

// Parse extracts RouteInfo from a raw request path and host.
// Supported path format:
// /{apiVersion}/projects/{projectID}/locations/{region}/publishers/{publisher}/models/{model}:{method}[?query]
// Example: /v1/projects/my-proj/locations/us-central1/publishers/google/models/gemini-1.5-pro:streamGenerateContent?alt=sse
func Parse(rawPath, host string) (*RouteInfo, error) {
	if rawPath == "" {
		return nil, ErrEmptyPath
	}

	path := rawPath
	var query string
	if qIdx := strings.IndexByte(rawPath, '?'); qIdx != -1 {
		path = rawPath[:qIdx]
		query = rawPath[qIdx+1:]
	}

	// Strip leading slash
	cleanPath := strings.TrimPrefix(path, "/")
	segments := strings.Split(cleanPath, "/")

	// Minimum path segments check: [v1, projects, p, locations, r, publishers, pub, models, model:method] -> 9 segments
	if len(segments) < 5 {
		return nil, fmt.Errorf("%w: path has too few segments (%d)", ErrNotAIPlatformPath, len(segments))
	}

	apiVersion := segments[0]
	if !strings.HasPrefix(apiVersion, "v1") {
		return nil, fmt.Errorf("%w: unsupported api version %q", ErrNotAIPlatformPath, apiVersion)
	}

	var projectID, region, publisher, modelWithMethod string

	for i := 1; i < len(segments); i += 2 {
		if i+1 >= len(segments) {
			break
		}
		key := segments[i]
		val := segments[i+1]

		switch key {
		case "projects":
			projectID = val
		case "locations":
			region = val
		case "publishers":
			publisher = val
		case "models":
			modelWithMethod = val
		}
	}

	if projectID == "" {
		return nil, ErrMissingProject
	}
	if region == "" {
		return nil, ErrMissingRegion
	}
	if modelWithMethod == "" {
		return nil, ErrMissingModel
	}

	// Split model and method by ':'
	colonIdx := strings.LastIndexByte(modelWithMethod, ':')
	if colonIdx == -1 {
		return nil, fmt.Errorf("%w: %q", ErrMissingMethod, modelWithMethod)
	}

	model := modelWithMethod[:colonIdx]
	method := modelWithMethod[colonIdx+1:]

	if model == "" {
		return nil, ErrMissingModel
	}
	if method == "" {
		return nil, ErrMissingMethod
	}

	if publisher == "" {
		publisher = "google"
	}

	return &RouteInfo{
		APIVersion:   apiVersion,
		ProjectID:    projectID,
		Region:       region,
		Publisher:    publisher,
		Model:        model,
		Method:       method,
		RawQuery:     query,
		Host:         host,
		OriginalPath: rawPath,
	}, nil
}
