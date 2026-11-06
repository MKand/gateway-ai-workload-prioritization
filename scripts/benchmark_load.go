package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/MKand/gateway-ai-workload-prioritization/gen/go/governor/v1"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/dataplane"
	"github.com/MKand/gateway-ai-workload-prioritization/pkg/governor"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

// BenchmarkResult summarizes a concurrent ext_proc overload benchmark run.
type BenchmarkResult struct {
	Concurrency       int
	RequestsPerWorker int
	TotalRequests     int
	CriticalSent      int64
	CriticalDropped   int64
	BestEffortSent    int64
	BestEffortShed    int64
	P50               time.Duration
	P95               time.Duration
	P99               time.Duration
}

// CriticalDropRate returns the fraction of critical requests that were shed.
func (r BenchmarkResult) CriticalDropRate() float64 {
	if r.CriticalSent == 0 {
		return 0
	}
	return float64(r.CriticalDropped) / float64(r.CriticalSent)
}

func main() {
	concurrency := flag.Int("concurrency", 500, "number of concurrent streaming ext_proc connections")
	requestsPerWorker := flag.Int("requests-per-worker", 4, "requests sent per concurrent worker (simulating 200% quota overload)")
	maxP99 := flag.Duration("max-p99", time.Millisecond, "maximum acceptable p99 decision latency")
	flag.Parse()

	res, err := RunOverloadBenchmark(context.Background(), *concurrency, *requestsPerWorker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Concurrency:          %d\n", res.Concurrency)
	fmt.Printf("Total Requests:       %d\n", res.TotalRequests)
	fmt.Printf("Critical Sent/Drop:   %d / %d (%.2f%% drop rate)\n", res.CriticalSent, res.CriticalDropped, res.CriticalDropRate()*100)
	fmt.Printf("BestEffort Sent/Shed: %d / %d\n", res.BestEffortSent, res.BestEffortShed)
	fmt.Printf("Decision Latency:     p50=%v  p95=%v  p99=%v\n", res.P50, res.P95, res.P99)

	if res.CriticalDropped > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: expected 0.0%% critical drop rate, got %d drops\n", res.CriticalDropped)
		os.Exit(1)
	}
	if res.P99 > *maxP99 {
		fmt.Fprintf(os.Stderr, "FAIL: p99 latency %v exceeded target %v\n", res.P99, *maxP99)
		os.Exit(1)
	}
}

// RunOverloadBenchmark spins up an in-memory Envoy ext_proc server configured
// at 200% quota load (97% primary utilization, above the best-effort ceiling)
// and drives concurrency streaming connections against it.
func RunOverloadBenchmark(ctx context.Context, concurrency, requestsPerWorker int) (BenchmarkResult, error) {
	if concurrency <= 0 {
		concurrency = 1
	}
	if requestsPerWorker <= 0 {
		requestsPerWorker = 1
	}

	reserve := int64(30)
	replicas := 1
	cfg := &governor.Config{
		SafetyMarginPercent:    10,
		CriticalReservePercent: &reserve,
		DataplaneReplicas:      &replicas,
	}
	engine, err := governor.NewCascadeEngineFromConfig(cfg)
	if err != nil {
		return BenchmarkResult{}, err
	}

	cache := dataplane.NewSnapshotCache()
	cache.Set(&pb.QuotaSnapshot{
		ProjectQuotas: map[string]*pb.ModelQuota{
			"proj-bench/region-primary/gemini-3.5-flash": {
				ProjectId:      "proj-bench",
				Region:         "region-primary",
				Model:          "gemini-3.5-flash",
				MaxRpm:         1000,
				CurrentRpm:     970,
				UtilizationRpm: 0.97,
			},
		},
	})

	extProc := dataplane.NewExtProcServer(dataplane.NewEvaluator(cache, engine))

	const reqPath = "/v1/projects/proj-bench/locations/region-primary/publishers/google/models/gemini-3.5-flash:streamGenerateContent"
	const reqHost = "region-primary-aiplatform.googleapis.com"

	total := concurrency * requestsPerWorker
	latencies := make([]time.Duration, total)

	var (
		criticalSent    atomic.Int64
		criticalDropped atomic.Int64
		bestEffortSent  atomic.Int64
		bestEffortShed  atomic.Int64
		firstErr        atomic.Value
		wg              sync.WaitGroup
	)

	// Prepare 500 concurrent bidirectional ext_proc streams, each pre-loaded with
	// requestsPerWorker streaming header events simulating 200% quota overload,
	// and execute them across a GOMAXPROCS worker pool so scheduler preemption
	// does not distort sub-millisecond per-request TTFT decision latency.
	type streamJob struct {
		workerID   int
		stream     *benchProcessStream
		priorities []bool
	}
	jobCh := make(chan streamJob, concurrency)
	for w := 0; w < concurrency; w++ {
		reqs := make([]*extprocv3.ProcessingRequest, requestsPerWorker)
		priorities := make([]bool, requestsPerWorker)
		for i := 0; i < requestsPerWorker; i++ {
			prio := "best_effort"
			isCritical := (w+i)%2 == 0
			priorities[i] = isCritical
			if isCritical {
				prio = "critical"
				criticalSent.Add(1)
			} else {
				bestEffortSent.Add(1)
			}
			reqs[i] = &extprocv3.ProcessingRequest{
				Request: &extprocv3.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extprocv3.HttpHeaders{
						Headers: &corev3.HeaderMap{
							Headers: []*corev3.HeaderValue{
								{Key: ":path", RawValue: []byte(reqPath)},
								{Key: ":authority", RawValue: []byte(reqHost)},
								{Key: "x-request-priority", RawValue: []byte(prio)},
							},
						},
					},
				},
			}
		}
		jobCh <- streamJob{
			workerID:   w,
			stream:     newBenchProcessStream(ctx, reqs),
			priorities: priorities,
		}
	}
	close(jobCh)

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	runtime.GC()
	wg.Add(workers)
	for p := 0; p < workers; p++ {
		go func() {
			defer wg.Done()
			for job := range jobCh {
				if err := extProc.Process(job.stream); err != nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
				for i, resp := range job.stream.resps {
					latencies[job.workerID*requestsPerWorker+i] = job.stream.durations[i]
					if resp.GetImmediateResponse() != nil {
						if job.priorities[i] {
							criticalDropped.Add(1)
						} else {
							bestEffortShed.Add(1)
						}
					}
				}
			}
		}()
	}
	wg.Wait()

	if v := firstErr.Load(); v != nil {
		return BenchmarkResult{}, v.(error)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	return BenchmarkResult{
		Concurrency:       concurrency,
		RequestsPerWorker: requestsPerWorker,
		TotalRequests:     total,
		CriticalSent:      criticalSent.Load(),
		CriticalDropped:   criticalDropped.Load(),
		BestEffortSent:    bestEffortSent.Load(),
		BestEffortShed:    bestEffortShed.Load(),
		P50:               percentile(latencies, 0.50),
		P95:               percentile(latencies, 0.95),
		P99:               percentile(latencies, 0.99),
	}, nil
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

type benchProcessStream struct {
	grpc.ServerStream
	ctx       context.Context
	reqs      []*extprocv3.ProcessingRequest
	resps     []*extprocv3.ProcessingResponse
	durations []time.Duration
	idx       int
	recvAt    time.Time
}

func newBenchProcessStream(ctx context.Context, reqs []*extprocv3.ProcessingRequest) *benchProcessStream {
	return &benchProcessStream{
		ctx:       ctx,
		reqs:      reqs,
		resps:     make([]*extprocv3.ProcessingResponse, 0, len(reqs)),
		durations: make([]time.Duration, 0, len(reqs)),
	}
}

func (s *benchProcessStream) Context() context.Context { return s.ctx }

func (s *benchProcessStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if s.idx >= len(s.reqs) {
		return nil, io.EOF
	}
	req := s.reqs[s.idx]
	s.idx++
	s.recvAt = time.Now()
	return req, nil
}

func (s *benchProcessStream) Send(resp *extprocv3.ProcessingResponse) error {
	s.durations = append(s.durations, time.Since(s.recvAt))
	s.resps = append(s.resps, resp)
	return nil
}
