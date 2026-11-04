package api

import (
	"context"
	"time"

	"github.com/runplane/runplane/internal/store"
)

type Overview struct {
	Range   string           `json:"range" enum:"24h,7d,30d"`
	Stats   OverviewStats    `json:"stats"`
	Running []JobSummary     `json:"running" doc:"All running jobs, newest first."`
	Servers []ServerSummary  `json:"servers" doc:"The primary first."`
	Buckets []OverviewBucket `json:"buckets" doc:"Completed jobs by queue time, oldest first, in UTC: 24 buckets of 1 hour for 24h, 28 of 6 hours for 7d, 30 of 1 day for 30d. The last bucket ends after now."`
	Recent  []JobSummary     `json:"recent" doc:"The last 10 completed jobs, newest first."`
}

// OverviewStats cover the jobs queued in the time of the buckets.
type OverviewStats struct {
	Jobs          int      `json:"jobs"`
	VCPUInUse     int      `json:"vcpu_in_use" doc:"vCPUs of the live sandboxes on the online and draining servers."`
	VCPUTotal     int      `json:"vcpu_total" doc:"vCPUs of the online and draining servers."`
	MedianQueueMS *int64   `json:"median_queue_ms" doc:"Median time from queued to assigned. Null when no job of the range has a runner yet."`
	FailureRate   float64  `json:"failure_rate" doc:"failed / (succeeded + failed), 0 to 1."`
	CacheHitRate  *float64 `json:"cache_hit_rate" doc:"Actions cache hits / (hits + misses), 0 to 1. Null when the range has no cache lookups."`
	DurationMS    int64    `json:"duration_ms" doc:"Sum of the run times of the completed jobs."`
}

type OverviewBucket struct {
	Start     time.Time `json:"start"`
	Succeeded int       `json:"succeeded"`
	Failed    int       `json:"failed"`
	Cancelled int       `json:"cancelled"`
}

type overviewInput struct {
	Range string `query:"range" enum:"24h,7d,30d" default:"24h"`
}

var bucketSizes = map[string]struct {
	size  time.Duration
	count int
}{
	"24h": {time.Hour, 24},
	"7d":  {6 * time.Hour, 28},
	"30d": {24 * time.Hour, 30},
}

func (h *Handler) getOverview(ctx context.Context, in *overviewInput) (*struct{ Body Overview }, error) {
	b := bucketSizes[in.Range]
	// The buckets start on whole hours, quarter days, or days in UTC, and
	// the last bucket has now.
	since := time.Now().UTC().Truncate(b.size).Add(-time.Duration(b.count-1) * b.size)
	until := since.Add(time.Duration(b.count) * b.size)
	o, err := h.d.Store.Overview(ctx, since, until, b.size)
	if err != nil {
		return nil, h.internal(err)
	}
	queueTimes, cacheLookups, err := h.d.Store.OverviewSamples(ctx, since, until)
	if err != nil {
		return nil, h.internal(err)
	}
	r, err := h.loadRefs(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	out := Overview{
		Range:   in.Range,
		Stats:   OverviewStats{Jobs: o.Jobs, FailureRate: o.FailureRate, DurationMS: o.DurationMS},
		Buckets: make([]OverviewBucket, len(o.Buckets)),
	}
	if queueTimes > 0 {
		out.Stats.MedianQueueMS = &o.MedianQueueMS
	}
	if cacheLookups > 0 {
		out.Stats.CacheHitRate = &o.CacheHitRate
	}
	for i, x := range o.Buckets {
		out.Buckets[i] = OverviewBucket(x)
	}
	if out.Running, err = h.jobSummaries(ctx, o.Running, r); err != nil {
		return nil, h.internal(err)
	}
	if out.Recent, err = h.jobSummaries(ctx, o.Recent, r); err != nil {
		return nil, h.internal(err)
	}
	list, err := h.d.Store.ListServers(ctx)
	if err != nil {
		return nil, h.internal(err)
	}
	servers, err := h.servers(ctx, list)
	if err != nil {
		return nil, h.internal(err)
	}
	out.Servers = make([]ServerSummary, len(servers))
	for i, v := range servers {
		out.Servers[i] = v.ServerSummary
		// An unreachable server keeps its sandbox rows until it connects
		// again, so its sandboxes count only with its vCPUs.
		if v.Status == store.ServerOnline || v.Status == store.ServerDraining {
			out.Stats.VCPUInUse += v.VCPUInUse
			out.Stats.VCPUTotal += v.VCPU
		}
	}
	return &struct{ Body Overview }{out}, nil
}
