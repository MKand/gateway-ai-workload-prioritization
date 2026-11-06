package router_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/router"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name          string
		rawPath       string
		host          string
		expected      *router.RouteInfo
		expectedError error
	}{
		{
			name:    "standard generateContent v1",
			rawPath: "/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent",
			host:    "us-central1-aiplatform.googleapis.com",
			expected: &router.RouteInfo{
				APIVersion:   "v1",
				ProjectID:    "my-project",
				Region:       "us-central1",
				Publisher:    "google",
				Model:        "gemini-1.5-pro",
				Method:       "generateContent",
				RawQuery:     "",
				Host:         "us-central1-aiplatform.googleapis.com",
				OriginalPath: "/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent",
			},
			expectedError: nil,
		},
		{
			name:    "streaming request with query params",
			rawPath: "/v1/projects/acme-corp-prod/locations/europe-west4/publishers/google/models/gemini-1.5-flash:streamGenerateContent?alt=sse",
			host:    "europe-west4-aiplatform.googleapis.com",
			expected: &router.RouteInfo{
				APIVersion:   "v1",
				ProjectID:    "acme-corp-prod",
				Region:       "europe-west4",
				Publisher:    "google",
				Model:        "gemini-1.5-flash",
				Method:       "streamGenerateContent",
				RawQuery:     "alt=sse",
				Host:         "europe-west4-aiplatform.googleapis.com",
				OriginalPath: "/v1/projects/acme-corp-prod/locations/europe-west4/publishers/google/models/gemini-1.5-flash:streamGenerateContent?alt=sse",
			},
			expectedError: nil,
		},
		{
			name:    "v1beta1 api version with countTokens",
			rawPath: "/v1beta1/projects/1234567890/locations/asia-northeast1/publishers/google/models/gemini-1.0-pro:countTokens",
			host:    "asia-northeast1-aiplatform.googleapis.com",
			expected: &router.RouteInfo{
				APIVersion:   "v1beta1",
				ProjectID:    "1234567890",
				Region:       "asia-northeast1",
				Publisher:    "google",
				Model:        "gemini-1.0-pro",
				Method:       "countTokens",
				RawQuery:     "",
				Host:         "asia-northeast1-aiplatform.googleapis.com",
				OriginalPath: "/v1beta1/projects/1234567890/locations/asia-northeast1/publishers/google/models/gemini-1.0-pro:countTokens",
			},
			expectedError: nil,
		},
		{
			name:          "empty path",
			rawPath:       "",
			host:          "us-central1-aiplatform.googleapis.com",
			expected:      nil,
			expectedError: router.ErrEmptyPath,
		},
		{
			name:          "non-ai platform path (healthz)",
			rawPath:       "/healthz",
			host:          "localhost",
			expected:      nil,
			expectedError: router.ErrNotAIPlatformPath,
		},
		{
			name:          "missing method colon",
			rawPath:       "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-1.5-pro",
			host:          "us-central1-aiplatform.googleapis.com",
			expected:      nil,
			expectedError: router.ErrMissingMethod,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := router.Parse(tc.rawPath, tc.host)
			if tc.expectedError != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tc.expectedError)
				}
				if !errors.Is(err, tc.expectedError) {
					t.Fatalf("expected error wrapping %v, got %v", tc.expectedError, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.APIVersion != tc.expected.APIVersion ||
				result.ProjectID != tc.expected.ProjectID ||
				result.Region != tc.expected.Region ||
				result.Publisher != tc.expected.Publisher ||
				result.Model != tc.expected.Model ||
				result.Method != tc.expected.Method ||
				result.RawQuery != tc.expected.RawQuery ||
				result.Host != tc.expected.Host {
				t.Errorf("mismatch:\ngot  %+v\nwant %+v", result, tc.expected)
			}
		})
	}
}

func TestRouteInfoHelpers(t *testing.T) {
	info := &router.RouteInfo{
		ProjectID: "my-proj",
		Region:    "us-central1",
		Model:     "gemini-1.5-pro",
		Method:    "streamGenerateContent",
	}

	if !info.IsStreaming() {
		t.Errorf("expected IsStreaming to be true")
	}

	if got := info.OrgKey(); got != "us-central1/gemini-1.5-pro" {
		t.Errorf("expected OrgKey 'us-central1/gemini-1.5-pro', got %q", got)
	}

	if got := info.ProjectKey(); got != "my-proj/us-central1/gemini-1.5-pro" {
		t.Errorf("expected ProjectKey 'my-proj/us-central1/gemini-1.5-pro', got %q", got)
	}
}

func TestMutate(t *testing.T) {
	original := &router.RouteInfo{
		APIVersion: "v1",
		ProjectID:  "my-project",
		Region:     "us-central1",
		Publisher:  "google",
		Model:      "gemini-1.5-pro",
		Method:     "streamGenerateContent",
		RawQuery:   "alt=sse",
		Host:       "us-central1-aiplatform.googleapis.com",
	}

	t.Run("model fallback only", func(t *testing.T) {
		newPath, newHost := router.Mutate(original, "gemini-1.5-flash", "")
		expectedPath := "/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-1.5-flash:streamGenerateContent?alt=sse"
		expectedHost := "us-central1-aiplatform.googleapis.com"

		if newPath != expectedPath {
			t.Errorf("expected path %q, got %q", expectedPath, newPath)
		}
		if newHost != expectedHost {
			t.Errorf("expected host %q, got %q", expectedHost, newHost)
		}
	})

	t.Run("regional failover only", func(t *testing.T) {
		newPath, newHost := router.Mutate(original, "", "us-east4")
		expectedPath := "/v1/projects/my-project/locations/us-east4/publishers/google/models/gemini-1.5-pro:streamGenerateContent?alt=sse"
		expectedHost := "us-east4-aiplatform.googleapis.com"

		if newPath != expectedPath {
			t.Errorf("expected path %q, got %q", expectedPath, newPath)
		}
		if newHost != expectedHost {
			t.Errorf("expected host %q, got %q", expectedHost, newHost)
		}
	})

	t.Run("combined model and regional failover", func(t *testing.T) {
		newPath, newHost := router.Mutate(original, "gemini-1.5-flash", "europe-west4")
		expectedPath := "/v1/projects/my-project/locations/europe-west4/publishers/google/models/gemini-1.5-flash:streamGenerateContent?alt=sse"
		expectedHost := "europe-west4-aiplatform.googleapis.com"

		if newPath != expectedPath {
			t.Errorf("expected path %q, got %q", expectedPath, newPath)
		}
		if newHost != expectedHost {
			t.Errorf("expected host %q, got %q", expectedHost, newHost)
		}
	})
}

func BenchmarkParse(b *testing.B) {
	rawPath := "/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-1.5-pro:streamGenerateContent?alt=sse"
	host := "us-central1-aiplatform.googleapis.com"

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = router.Parse(rawPath, host)
	}
}

func BenchmarkMutate(b *testing.B) {
	info := &router.RouteInfo{
		APIVersion: "v1",
		ProjectID:  "my-project",
		Region:     "us-central1",
		Publisher:  "google",
		Model:      "gemini-1.5-pro",
		Method:     "streamGenerateContent",
		RawQuery:   "alt=sse",
		Host:       "us-central1-aiplatform.googleapis.com",
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = router.Mutate(info, "gemini-1.5-flash", "us-east4")
	}
}

func TestParse_GlobalAndPublisher(t *testing.T) {
	const globalPath = "/v1/projects/p1/locations/global/publishers/google/models/gemini-3.5-flash:generateContent"
	info, err := router.Parse(globalPath, "aiplatform.googleapis.com")
	if err != nil {
		t.Fatalf("global path: %v", err)
	}
	if info.Region != "global" || info.Model != "gemini-3.5-flash" || info.ProjectID != "p1" {
		t.Fatalf("unexpected route info %+v", info)
	}

	for _, pub := range []string{"anthropic", "meta"} {
		p := "/v1/projects/p1/locations/us-east5/publishers/" + pub + "/models/claude:rawPredict"
		if _, err := router.Parse(p, "us-east5-aiplatform.googleapis.com"); !errors.Is(err, router.ErrUnsupportedPublisher) {
			t.Errorf("publisher %s: err = %v, want ErrUnsupportedPublisher", pub, err)
		}
	}
	if _, err := router.Parse("/v1/projects/p1/locations/us-central1/publishers/Google/models/gemini-3.5-flash:generateContent", ""); err != nil {
		t.Errorf("publisher match should be case-insensitive: %v", err)
	}
}

func TestMutateHost_GlobalAndCustomHosts(t *testing.T) {
	base := func(region, host string) *router.RouteInfo {
		return &router.RouteInfo{APIVersion: "v1", ProjectID: "p1", Region: region, Publisher: "google", Model: "m", Method: "generateContent", Host: host}
	}
	tests := []struct {
		name   string
		info   *router.RouteInfo
		target string
		want   string
	}{
		{"global to regional", base("global", "aiplatform.googleapis.com"), "europe-west4", "europe-west4-aiplatform.googleapis.com"},
		{"regional to global", base("us-central1", "us-central1-aiplatform.googleapis.com"), "global", "aiplatform.googleapis.com"},
		{"global host with port", base("global", "aiplatform.googleapis.com:443"), "us-east1", "us-east1-aiplatform.googleapis.com"},
		{"same region keeps host", base("global", "aiplatform.googleapis.com"), "GLOBAL", "aiplatform.googleapis.com"},
		{"custom private host kept", base("us-central1", "vertex.internal.example.com"), "global", "vertex.internal.example.com"},
		{"lookalike host kept", base("us-central1", "evil-aiplatform.googleapis.com.example.com"), "global", "evil-aiplatform.googleapis.com.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := router.MutateHost(tc.info, tc.target); got != tc.want {
				t.Errorf("MutateHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMutate_ProjectNeverChanges(t *testing.T) {
	info := &router.RouteInfo{APIVersion: "v1", ProjectID: "caller-project", Region: "global", Publisher: "google", Model: "m", Method: "generateContent", Host: "aiplatform.googleapis.com"}
	for _, target := range []string{"global", "us-central1", "europe-west4"} {
		p, _ := router.Mutate(info, "other-model", target)
		if !strings.Contains(p, "/projects/caller-project/") {
			t.Errorf("reroute to %s changed the project: %s", target, p)
		}
	}
}

func TestHostFor(t *testing.T) {
	if got := router.HostFor("global"); got != "aiplatform.googleapis.com" {
		t.Errorf("HostFor(global) = %q", got)
	}
	if got := router.HostFor("US-Central1"); got != "us-central1-aiplatform.googleapis.com" {
		t.Errorf("HostFor(US-Central1) = %q", got)
	}
}
