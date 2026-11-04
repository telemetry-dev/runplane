package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Overview has the numbers of the jobs queued in [since, until).
type Overview struct {
	Jobs          int              `json:"jobs"`
	MedianQueueMS int64            `json:"median_queue_ms"` // queued to assigned (or to started)
	FailureRate   float64          `json:"failure_rate"`    // failed / (succeeded + failed)
	CacheHitRate  float64          `json:"cache_hit_rate"`  // hits / (hits + misses)
	DurationMS    int64            `json:"duration_ms"`     // sum of the run times of completed jobs
	Buckets       []OverviewBucket `json:"buckets"`
	Running       []Job            `json:"running"` // all running jobs, newest first
	Recent        []Job            `json:"recent"`  // the last completed jobs
}

// OverviewBucket counts the completed jobs queued in [Start, Start+bucket).
type OverviewBucket struct {
	Start     time.Time `json:"start"`
	Succeeded int       `json:"succeeded"`
	Failed    int       `json:"failed"`
	Cancelled int       `json:"cancelled"`
}

const recentJobs = 10

// Overview computes the dashboard numbers for [since, until). The buckets
// start at since; the last bucket can end after until.
func (s *Store) Overview(ctx context.Context, since, until time.Time, bucket time.Duration) (Overview, error) {
	if bucket <= 0 || !until.After(since) {
		return Overview{}, errors.New("overview: bad range")
	}
	o, err := s.overview(ctx, since, until, bucket)
	if err != nil {
		return Overview{}, fmt.Errorf("overview: %w", err)
	}
	return o, nil
}

func (s *Store) overview(ctx context.Context, since, until time.Time, bucket time.Duration) (Overview, error) {
	from, to := ms(since), ms(until)
	var o Overview
	var failed, decided int
	err := s.r.QueryRowContext(ctx, `SELECT count(*),
			count(*) FILTER (WHERE result = 'failed'),
			count(*) FILTER (WHERE result IN ('succeeded', 'failed')),
			coalesce(sum(finished_at - started_at) FILTER (WHERE state = 'completed'), 0)
		FROM jobs WHERE queued_at >= ? AND queued_at < ?`, from, to).
		Scan(&o.Jobs, &failed, &decided, &o.DurationMS)
	if err != nil {
		return Overview{}, err
	}
	if decided > 0 {
		o.FailureRate = float64(failed) / float64(decided)
	}

	rows, err := s.r.QueryContext(ctx, `SELECT max(coalesce(assigned_at, started_at) - queued_at, 0) FROM jobs
		WHERE queued_at >= ? AND queued_at < ? AND coalesce(assigned_at, started_at) IS NOT NULL`, from, to)
	waits, err := scanAll(rows, err, scanInt64)
	if err != nil {
		return Overview{}, err
	}
	o.MedianQueueMS = median(waits)

	// cache_stats has one row per hour: take the hours that overlap the range.
	var hits, misses int64
	err = s.r.QueryRowContext(ctx, `SELECT coalesce(sum(hits), 0), coalesce(sum(misses), 0) FROM cache_stats
		WHERE hour >= ? AND hour < ?`, since.Unix()/3600, (until.Unix()+3599)/3600).Scan(&hits, &misses)
	if err != nil {
		return Overview{}, err
	}
	if hits+misses > 0 {
		o.CacheHitRate = float64(hits) / float64(hits+misses)
	}

	if o.Buckets, err = s.buckets(ctx, since, until, bucket); err != nil {
		return Overview{}, err
	}

	rows, err = s.r.QueryContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE state = 'running' ORDER BY queued_at DESC, id DESC")
	if o.Running, err = scanAll(rows, err, scanJob); err != nil {
		return Overview{}, err
	}
	rows, err = s.r.QueryContext(ctx, "SELECT "+jobCols+` FROM jobs WHERE state = 'completed'
		ORDER BY finished_at DESC, id DESC LIMIT ?`, recentJobs)
	if o.Recent, err = scanAll(rows, err, scanJob); err != nil {
		return Overview{}, err
	}
	return o, nil
}

func (s *Store) buckets(ctx context.Context, since, until time.Time, bucket time.Duration) ([]OverviewBucket, error) {
	n := int((until.Sub(since) + bucket - 1) / bucket)
	list := make([]OverviewBucket, n)
	for i := range list {
		list[i].Start = since.Add(time.Duration(i) * bucket).UTC()
	}
	type count struct {
		i, n   int
		result string
	}
	rows, err := s.r.QueryContext(ctx, `SELECT (queued_at - ?1) / ?2, result, count(*) FROM jobs
		WHERE state = 'completed' AND result IN ('succeeded', 'failed', 'cancelled') AND queued_at >= ?1 AND queued_at < ?3
		GROUP BY 1, 2`, ms(since), bucket.Milliseconds(), ms(until))
	counts, err := scanAll(rows, err, func(sc scanner) (count, error) {
		var c count
		return c, sc.Scan(&c.i, &c.result, &c.n)
	})
	if err != nil {
		return nil, err
	}
	for _, c := range counts {
		b := &list[c.i]
		switch c.result {
		case ResultSucceeded:
			b.Succeeded = c.n
		case ResultFailed:
			b.Failed = c.n
		case ResultCancelled:
			b.Cancelled = c.n
		}
	}
	return list, nil
}
