package controlplane

import (
	"testing"

	"cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
)

func dimInfo(region, model string, value int64) *cloudquotaspb.DimensionsInfo {
	dims := map[string]string{}
	if region != "" {
		dims["region"] = region
	}
	if model != "" {
		dims["base_model"] = model
	}
	return &cloudquotaspb.DimensionsInfo{Dimensions: dims, Details: &cloudquotaspb.QuotaDetails{Value: value}}
}

// Regression test for a bug found in the live test: Cloud Quotas returns a
// specific {region, base_model} override followed by region-wide entries, and
// the region-wide entry used to overwrite the override.
func TestExtractLimits_MostSpecificWins(t *testing.T) {
	regions := []string{"us-central1", "europe-west1"}
	models := []string{"gemini-tts", "gemini-other"}

	entries := []*cloudquotaspb.DimensionsInfo{
		dimInfo("", "", 5),                       // global
		dimInfo("us-central1", "", 0),            // region-wide, no value
		dimInfo("", "gemini-tts", 50),            // model-wide
		dimInfo("us-central1", "gemini-tts", 10), // exact override
		dimInfo("europe-west1", "", 7),           // region-wide
	}
	want := map[string]int64{
		"us-central1/gemini-tts":    10, // exact beats everything
		"europe-west1/gemini-tts":   50, // model-wide beats region-wide
		"us-central1/gemini-other":  0,  // region-wide beats global
		"europe-west1/gemini-other": 7,
	}

	// Order must not matter: check forward and reversed.
	reversed := make([]*cloudquotaspb.DimensionsInfo, len(entries))
	for i, e := range entries {
		reversed[len(entries)-1-i] = e
	}
	for name, list := range map[string][]*cloudquotaspb.DimensionsInfo{"forward": entries, "reversed": reversed} {
		t.Run(name, func(t *testing.T) {
			got := extractLimits(&cloudquotaspb.QuotaInfo{DimensionsInfos: list}, regions, models)
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %d, want %d", k, got[k], v)
				}
			}
		})
	}
}

func TestExtractLimits_SkipsEntriesWithoutDetails(t *testing.T) {
	qi := &cloudquotaspb.QuotaInfo{DimensionsInfos: []*cloudquotaspb.DimensionsInfo{
		{Dimensions: map[string]string{"region": "us-central1"}},
	}}
	if got := extractLimits(qi, []string{"us-central1"}, []string{"m"}); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestResolveLimit(t *testing.T) {
	limits := map[string]int64{"r/unlimited": UnlimitedQuota, "r/set": 10, "r/zero": 0}
	cases := map[string]int64{
		"r/unlimited": 0,    // unlimited -> 0 ("no limit"), not the fallback
		"r/set":       10,   // real value
		"r/zero":      1000, // zero -> fallback
		"r/missing":   1000, // absent -> fallback
	}
	for key, want := range cases {
		if got := resolveLimit(limits, key, 1000); got != want {
			t.Errorf("resolveLimit(%s) = %d, want %d", key, got, want)
		}
	}
}
