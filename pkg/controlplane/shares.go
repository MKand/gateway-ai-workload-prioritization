package controlplane

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
)

const (
	// DefaultShareTTL is how long a data plane replica counts as active after
	// its last ReportUsage call.
	DefaultShareTTL = 30 * time.Second

	// DefaultShareFloor is the part of each allowance (0-1) that is split
	// evenly across replicas regardless of demand, so an idle replica can still
	// absorb a sudden burst. 0.1 means each of N replicas always gets at least
	// 10%/N of the allowance.
	DefaultShareFloor = 0.1

	// demandSmoothing is the weight of the newest report in the moving average
	// of each replica's demand. Smoothing stops shares from jumping around on
	// every report.
	demandSmoothing = 0.5
)

type replicaDemand struct {
	lastSeen time.Time
	requests map[string]float64 // target -> smoothed requests per report
	tokens   map[string]float64 // target -> smoothed tokens per report
}

// ShareTracker splits each allowance between data plane replicas in
// proportion to the demand they report, so a replica that receives more
// traffic gets a larger slice. It is safe for concurrent use.
type ShareTracker struct {
	mu       sync.Mutex
	replicas map[string]*replicaDemand
	now      func() time.Time
	ttl      time.Duration
	floor    float64
}

// NewShareTracker returns a ShareTracker with the default TTL and floor.
// clock may be nil (time.Now).
func NewShareTracker(clock func() time.Time) *ShareTracker {
	if clock == nil {
		clock = time.Now
	}
	return &ShareTracker{
		replicas: make(map[string]*replicaDemand),
		now:      clock,
		ttl:      DefaultShareTTL,
		floor:    DefaultShareFloor,
	}
}

// Report records one replica's demand since its previous report and returns
// that replica's share of every target any active replica has reported, plus
// the number of active replicas.
func (t *ShareTracker) Report(clientID string, demand []*pb.TargetDemand) ([]*pb.TargetShare, int32, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil, 0, errors.New("client_id cannot be empty")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	for id, r := range t.replicas {
		if id != clientID && now.Sub(r.lastSeen) > t.ttl {
			delete(t.replicas, id)
		}
	}

	r, ok := t.replicas[clientID]
	if !ok {
		r = &replicaDemand{requests: map[string]float64{}, tokens: map[string]float64{}}
		t.replicas[clientID] = r
	}
	latestReq := map[string]float64{}
	latestTok := map[string]float64{}
	for _, d := range demand {
		if d == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(d.GetTarget()))
		if key == "" {
			continue
		}
		latestReq[key] += float64(max(d.GetRequests(), 0))
		latestTok[key] += float64(max(d.GetTokens(), 0))
	}
	smooth(r.requests, latestReq, !ok)
	smooth(r.tokens, latestTok, !ok)
	r.lastSeen = now

	targets := map[string]bool{}
	for _, rep := range t.replicas {
		for k := range rep.requests {
			targets[k] = true
		}
		for k := range rep.tokens {
			targets[k] = true
		}
	}
	keys := make([]string, 0, len(targets))
	for k := range targets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	shares := make([]*pb.TargetShare, 0, len(keys))
	for _, k := range keys {
		shares = append(shares, &pb.TargetShare{
			Target:      k,
			RpmFraction: t.fraction(clientID, k, func(rep *replicaDemand) map[string]float64 { return rep.requests }),
			TpmFraction: t.fraction(clientID, k, func(rep *replicaDemand) map[string]float64 { return rep.tokens }),
		})
	}
	return shares, int32(len(t.replicas)), nil
}

// smooth folds the latest report into the moving average. Targets missing
// from the latest report decay towards zero. The first report is taken as is.
func smooth(avg, latest map[string]float64, first bool) {
	for k, v := range latest {
		if first {
			avg[k] = v
			continue
		}
		avg[k] = demandSmoothing*v + (1-demandSmoothing)*avg[k]
	}
	for k := range avg {
		if _, ok := latest[k]; !ok && !first {
			avg[k] *= 1 - demandSmoothing
		}
	}
}

// fraction returns clientID's share of target: an even floor plus the rest
// split in proportion to demand. Shares across replicas sum to 1.
func (t *ShareTracker) fraction(clientID, target string, metric func(*replicaDemand) map[string]float64) float64 {
	n := float64(len(t.replicas))
	if n == 0 {
		return 1
	}
	var total float64
	for _, rep := range t.replicas {
		total += metric(rep)[target]
	}
	if total <= 0 {
		return 1 / n
	}
	mine := metric(t.replicas[clientID])[target]
	return t.floor/n + (1-t.floor)*mine/total
}
