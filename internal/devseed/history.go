//go:build devseed

package devseed

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// run is one workflow run.
type run struct {
	repo    *repoSpec
	wf      *workflow
	at      time.Time
	number  int64
	id      int64
	branch  string
	pr      int
	event   string
	actor   string
	sha     string
	message string
	jobs    []*job
}

type job struct {
	run                                 *run
	kind                                *jobKind
	queued, assigned, started, finished time.Time // zero until the job gets there
	result                              string    // "" until completed
	server                              *server   // nil while queued
	steps                               []store.JobStep
	main                                [2]time.Time   // planned start and end of the main step
	design                              *designMetrics // the metrics that a design shows, nil for others
	requestID, githubID                 int64
	sandbox                             *store.Sandbox
	id                                  string // set by the store
}

func (j *job) state() string {
	switch {
	case j.result != "":
		return store.JobCompleted
	case !j.started.IsZero():
		return store.JobRunning
	}
	return store.JobQueued
}

// history writes about 1,300 jobs of the last 30 days, their sandboxes, the
// warm sandboxes, the scale sets, and the metrics and logs of the newest jobs.
func (s *seeder) history(ctx context.Context) error {
	// The design jobs fill the last 40 minutes.
	runs := s.background(s.now.Add(-30*24*time.Hour), s.now.Add(-40*time.Minute))
	fixtures, err := s.fixtures()
	if err != nil {
		return err
	}
	runs = append(runs, fixtures...)
	slices.SortStableFunc(runs, func(a, b *run) int { return a.at.Compare(b.at) })
	numberRuns(runs)

	// The run, job, runner, and request IDs are in the ranges that GitHub uses.
	var jobs []*job
	runID := int64(18_934_221_007)
	for _, r := range runs {
		runID += 2_000 + s.rng.Int64N(9_000)
		r.id = runID
		jobs = append(jobs, r.jobs...)
	}
	slices.SortStableFunc(jobs, func(a, b *job) int { return a.queued.Compare(b.queued) })
	githubID, runnerID := int64(53_118_400_000), int64(1_520_000)
	for i, j := range jobs {
		j.requestID = 3_400_000 + int64(i)
		githubID += 20 + s.rng.Int64N(400)
		j.githubID = githubID
		if j.server != nil {
			runnerID++
			j.sandbox = s.sandbox(j, runnerID)
		}
	}

	warm := make([]store.Sandbox, len(warmSandboxes))
	for i, w := range warmSandboxes {
		runnerID++
		warm[i] = s.warmSandbox(w.pool, s.servers[w.server], s.now.Add(-w.age), runnerID)
	}
	if err := s.addScaleSets(ctx, liveStats(jobs)); err != nil {
		return err
	}
	for _, j := range jobs {
		if err := s.writeJob(ctx, j); err != nil {
			return err
		}
	}
	for i, w := range warmSandboxes {
		warm[i].ScaleSetID = s.scaleSets[scaleSetKey{pool: w.pool}]
		if err := s.st.InsertSandbox(ctx, warm[i]); err != nil {
			return err
		}
	}
	if err := s.telemetry(ctx, jobs); err != nil {
		return err
	}
	return s.cacheStats(ctx, jobs)
}

// warmSandboxes are the idle sandboxes that wait for jobs.
var warmSandboxes = []struct {
	pool, server string
	age          time.Duration
}{
	{"runplane-2cpu", "fsn1-ax52-b", 6*time.Minute + 12*time.Second},
	{"runplane-2cpu", "fsn1-ax102-a", 2*time.Minute + 40*time.Second},
}

// background makes the runs of [start, cutoff): pushes with the daily rhythm
// of hourWeights, fewer on weekends, and the nightly schedules.
func (s *seeder) background(start, cutoff time.Time) []*run {
	rate := func(t time.Time) float64 {
		w := hourWeights[t.Hour()]
		if d := t.Weekday(); d == time.Saturday || d == time.Sunday {
			w *= weekendFactor
		}
		return w
	}
	first := start.Truncate(time.Hour)
	var total, repoWeights float64
	for t := first; t.Before(cutoff); t = t.Add(time.Hour) {
		total += rate(t)
	}
	for _, r := range repoSpecs {
		repoWeights += r.pushes
	}

	// Spread the pushes over the hours by their expected count, so that each
	// day shows the rhythm instead of random gaps.
	var runs []*run
	due := s.rng.Float64()
	for t := first; t.Before(cutoff); t = t.Add(time.Hour) {
		due += pushes * rate(t) / total
		n := int(due)
		due -= float64(n)
		for range n {
			at := t.Add(time.Duration(s.rng.Int64N(int64(time.Hour))))
			repo := s.pickRepo(repoWeights)
			if !at.Before(start) && at.Before(cutoff) {
				runs = append(runs, s.push(repo, at)...)
			}
		}
		if t.Hour() == 3 {
			at := t.Add(time.Duration(2+s.rng.IntN(8)) * time.Minute)
			if !at.Before(start) && at.Before(cutoff) {
				runs = append(runs, s.nightly(at)...)
			}
		}
	}
	return runs
}

func (s *seeder) pickRepo(total float64) *repoSpec {
	x := s.rng.Float64() * total
	for _, r := range repoSpecs {
		if x -= r.pushes; x < 0 {
			return r
		}
	}
	return repoSpecs[len(repoSpecs)-1]
}

// push makes the runs that one push to repo starts.
func (s *seeder) push(repo *repoSpec, at time.Time) []*run {
	base := run{
		repo: repo, branch: "main", event: "push", actor: pick(s, repo.actors),
		sha: s.hex(40), message: pick(s, repo.messages),
	}
	pr := len(repo.branches) > 0 && s.rng.Float64() < repo.prShare
	if pr {
		b := pick(s, repo.branches)
		base.branch, base.pr, base.event, base.actor = b.name, b.pr, "pull_request", b.actor
	}
	var runs []*run
	for _, wf := range repo.workflows {
		if pr && !wf.onPR || !pr && s.rng.Float64() >= wf.onPush {
			continue
		}
		r := base
		r.wf = wf
		r.at = at.Add(time.Duration(s.rng.IntN(1500)) * time.Millisecond)
		s.runJobs(&r)
		runs = append(runs, &r)
	}
	return runs
}

// nightly makes the scheduled runs on main.
func (s *seeder) nightly(at time.Time) []*run {
	var runs []*run
	for _, repo := range repoSpecs {
		for _, wf := range repo.workflows {
			if !wf.nightly {
				continue
			}
			r := &run{
				repo: repo, wf: wf, at: at.Add(time.Duration(s.rng.IntN(30)) * time.Second),
				branch: "main", event: "schedule", actor: repo.actors[0], sha: s.hex(40), message: pick(s, repo.messages),
			}
			s.runJobs(r)
			runs = append(runs, r)
		}
	}
	return runs
}

// runJobs makes the completed jobs of r. A job that needs an earlier job
// starts after that job succeeds.
func (s *seeder) runJobs(r *run) {
	done := map[string]*job{}
	for _, k := range r.wf.jobs {
		queued := r.at.Add(time.Duration(s.rng.IntN(800)) * time.Millisecond)
		if k.after != "" {
			prev := done[k.after]
			if prev == nil || prev.result != store.ResultSucceeded {
				continue
			}
			queued = prev.finished.Add(time.Duration(1500+s.rng.IntN(2000)) * time.Millisecond)
		}
		j := &job{run: r, kind: k, queued: queued}
		s.complete(j)
		r.jobs = append(r.jobs, j)
		done[k.name] = j
	}
}

// complete gives a background job its server, times, result, and steps.
func (s *seeder) complete(j *job) {
	j.server = s.place(j.kind.pool, j.queued)
	j.assigned = j.queued.Add(s.queueTime())
	j.started = j.assigned.Add(time.Duration(300+s.rng.IntN(700)) * time.Millisecond)
	j.result = store.ResultSucceeded
	d := s.jitter(seconds(j.kind.secs), 0.15)
	cancel := 0.005
	if j.run.event == "pull_request" {
		cancel = 0.015 // a newer push to the pull request cancels the run
	}
	switch u := s.rng.Float64(); {
	case u < j.kind.fail:
		j.result = store.ResultFailed
		d = time.Duration(float64(d) * (0.5 + 0.55*s.rng.Float64()))
	case u < j.kind.fail+cancel:
		j.result = store.ResultCancelled
		d = time.Duration(float64(d) * (0.1 + 0.5*s.rng.Float64()))
	}
	end := s.layout(j, s.stepLengths(j.kind, d, j.result))
	j.finished = end.Add(time.Duration(1000+s.rng.IntN(1500)) * time.Millisecond)
}

// queueTime is the time from queued to assigned: a few seconds with a warm
// sandbox, longer when a sandbox must start first.
func (s *seeder) queueTime() time.Duration {
	if s.rng.Float64() < 0.12 {
		return time.Duration(8_000+s.rng.IntN(50_000)) * time.Millisecond
	}
	return time.Duration(1_000+s.rng.IntN(3_200)) * time.Millisecond
}

// place picks a server for a sandbox of the pool, more often the larger ones.
func (s *seeder) place(label string, at time.Time) *server {
	p := s.pools[label]
	var list []*server
	total := 0
	for _, spec := range serverSpecs {
		v := s.servers[spec.name]
		online := spec.status == store.ServerOnline ||
			spec.status == store.ServerUnreachable && at.Before(s.now.Add(-spec.seen))
		if !online || spec.facts.Arch != p.Arch || spec.facts.VCPU-1 < p.VCPU {
			continue
		}
		list = append(list, v)
		total += spec.facts.VCPU
	}
	n := s.rng.IntN(total)
	for _, v := range list {
		if n -= v.facts.VCPU; n < 0 {
			return v
		}
	}
	return list[len(list)-1]
}

// stepLengths returns the length in seconds of each step of a job that takes
// about d. After a failed or cancelled main step, the steps up to "Complete
// job" are skipped and have length 0.
func (s *seeder) stepLengths(k *jobKind, d time.Duration, result string) []int {
	main := mainStep(k.steps)
	lengths := make([]int, len(k.steps))
	sum := 0
	for i, st := range k.steps {
		if i == main || i > main && i < len(k.steps)-1 && result != store.ResultSucceeded {
			continue
		}
		lengths[i] = max(1, int(math.Round(st.secs*(0.8+0.45*s.rng.Float64()))))
		sum += lengths[i]
	}
	lengths[main] = max(1, int(d.Seconds())-sum-3)
	return lengths
}

// layout places the steps of j back to back from just after its start, with
// whole-second times as GitHub reports them, and returns the end of the
// last step.
func (s *seeder) layout(j *job, lengths []int) time.Time {
	main := mainStep(j.kind.steps)
	at := j.started.Truncate(time.Second).Add(time.Duration(1+s.rng.IntN(2)) * time.Second)
	j.steps = make([]store.JobStep, len(lengths))
	for i, n := range lengths {
		end := at.Add(time.Duration(n) * time.Second)
		conclusion := "success"
		switch {
		case i == main && j.result == store.ResultFailed:
			conclusion = "failure"
		case i == main && j.result == store.ResultCancelled:
			conclusion = "cancelled"
		case n == 0:
			conclusion = "skipped"
		}
		if i == main {
			j.main = [2]time.Time{at, end}
		}
		j.steps[i] = store.JobStep{
			Number: i + 1, Name: j.kind.steps[i].name, Status: "completed", Conclusion: conclusion,
			StartedAt: ptr(at), CompletedAt: ptr(end),
		}
		at = end
	}
	return at
}

// progress marks the steps of a running job that have not ended by now.
func (s *seeder) progress(j *job) {
	for i := range j.steps {
		st := &j.steps[i]
		switch {
		case !st.CompletedAt.After(s.now):
		case !st.StartedAt.After(s.now):
			st.Status, st.Conclusion, st.CompletedAt = "in_progress", "", nil
		default:
			st.Status, st.Conclusion, st.StartedAt, st.CompletedAt = "queued", "", nil, nil
		}
	}
}

func mainStep(steps []step) int {
	return slices.IndexFunc(steps, func(st step) bool { return st.secs == 0 })
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// fixtures makes the runs of the designs. Their times are whole seconds
// before now, so the durations match the designs.
func (s *seeder) fixtures() ([]*run, error) {
	now := s.now.Truncate(time.Second)
	var runs []*run
	for _, f := range fixtureRuns {
		repo, wf, err := findWorkflow(f.repo, f.file)
		if err != nil {
			return nil, err
		}
		r := &run{
			repo: repo, wf: wf, number: f.number, branch: f.branch, pr: f.pr, event: "push",
			actor: f.actor, sha: f.sha + s.hex(33), message: f.message,
		}
		if f.pr > 0 {
			r.event = "pull_request"
		}
		for _, fj := range f.jobs {
			i := slices.IndexFunc(wf.jobs, func(k *jobKind) bool { return k.name == fj.name })
			if i < 0 {
				return nil, fmt.Errorf("fixture: %s %s has no job %q", f.repo, f.file, fj.name)
			}
			j := &job{run: r, kind: wf.jobs[i], design: fj.metrics}
			switch {
			case fj.server == "":
				j.queued = now.Add(-fj.ago)
			case fj.dur == 0:
				s.startFixture(j, fj, now)
				s.layout(j, s.stepLengths(j.kind, seconds(j.kind.secs), store.ResultSucceeded))
				s.progress(j)
			default:
				s.startFixture(j, fj, now)
				j.result = fj.result
				j.finished = j.started.Add(fj.dur)
				lengths := fj.steps
				if lengths == nil {
					lengths = s.stepLengths(j.kind, fj.dur, fj.result)
				}
				s.layout(j, lengths)
			}
			r.jobs = append(r.jobs, j)
			if r.at.IsZero() || j.queued.Before(r.at) {
				r.at = j.queued
			}
		}
		runs = append(runs, r)
	}
	return runs, nil
}

func (s *seeder) startFixture(j *job, fj fixtureJob, now time.Time) {
	j.server = s.servers[fj.server]
	j.started = now.Add(-fj.ago)
	j.assigned = j.started.Add(-400 * time.Millisecond)
	j.queued = j.assigned.Add(-fj.queue)
}

func findWorkflow(repoName, file string) (*repoSpec, *workflow, error) {
	for _, r := range repoSpecs {
		if r.name != repoName {
			continue
		}
		for _, wf := range r.workflows {
			if wf.file == file {
				return r, wf, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("fixture: unknown workflow %s %s", repoName, file)
}

// numberRuns gives each background run a run number. The numbers count down
// from the oldest design run of the same workflow; runs is in time order.
func numberRuns(runs []*run) {
	next := map[*workflow]int64{}
	for _, r := range runs {
		if n, ok := next[r.wf]; r.number > 0 && (!ok || r.number <= n) {
			next[r.wf] = r.number - 1
		}
	}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.number > 0 {
			continue
		}
		if _, ok := next[r.wf]; !ok {
			next[r.wf] = 57 // a workflow that no design shows
		}
		r.number = next[r.wf]
		next[r.wf]--
	}
}

// sandbox returns the sandbox that ran j. A job that waited only a few
// seconds got a warm sandbox, which started minutes before the job.
func (s *seeder) sandbox(j *job, runnerID int64) *store.Sandbox {
	created := j.queued.Add(300 * time.Millisecond)
	idle := j.assigned.Add(-500 * time.Millisecond)
	if j.assigned.Sub(j.queued) < 6*time.Second {
		created = j.queued.Add(-time.Duration(60+s.rng.IntN(1500)) * time.Second)
		idle = created.Add(time.Duration(4000+s.rng.IntN(3000)) * time.Millisecond)
	}
	b := s.newSandbox(j.kind.pool, j.server, created, runnerID)
	b.State = sandbox.StateBusy
	b.IdleAt, b.BusyAt = ptr(idle), ptr(j.assigned)
	if j.started.Before(s.now.Add(-8 * 24 * time.Hour)) {
		b.RunnerVersion = oldRunnerVersion
	}
	if j.result != "" {
		b.State = sandbox.StateGone
		b.StoppedAt = ptr(j.finished.Add(time.Duration(1000+s.rng.IntN(2000)) * time.Millisecond))
		b.ExitReason = "runner exited"
	}
	return &b
}

func (s *seeder) warmSandbox(label string, v *server, created time.Time, runnerID int64) store.Sandbox {
	b := s.newSandbox(label, v, created, runnerID)
	b.State = sandbox.StateIdle
	b.IdleAt = ptr(created.Add(time.Duration(4000+s.rng.IntN(3000)) * time.Millisecond))
	return b
}

func (s *seeder) newSandbox(label string, v *server, created time.Time, runnerID int64) store.Sandbox {
	p := s.pools[label]
	id := s.id(created)
	ip := fmt.Sprintf("10.200.%d.%d", s.rng.IntN(64), 2+4*s.rng.IntN(63))
	if v.facts.Mode == sandbox.ModeContainer {
		ip = fmt.Sprintf("172.30.%d.%d", s.rng.IntN(4), 2+s.rng.IntN(250))
	}
	return store.Sandbox{
		ID: id, PoolID: p.ID, ServerID: v.id, Mode: v.facts.Mode, RunnerID: runnerID,
		RunnerName: label + "-" + id[len(id)-6:], IP: ip, ImageDigest: s.digests[p.Arch],
		RunnerVersion: runnerVersion, VCPU: p.VCPU, MemoryMB: p.MemoryMB, CreatedAt: created,
	}
}

// liveStats counts the live jobs and runners of each organization scale set.
func liveStats(jobs []*job) map[string]store.ScaleSetStats {
	stats := map[string]store.ScaleSetStats{}
	for _, j := range jobs {
		if j.run.repo.personal() {
			continue
		}
		x := stats[j.kind.pool]
		switch j.state() {
		case store.JobRunning:
			x.AssignedJobs++
			x.RunningJobs++
			x.RegisteredRunners++
			x.BusyRunners++
		case store.JobQueued:
			x.AcquiredJobs++
		}
		stats[j.kind.pool] = x
	}
	for _, w := range warmSandboxes {
		x := stats[w.pool]
		x.RegisteredRunners++
		x.IdleRunners++
		stats[w.pool] = x
	}
	return stats
}

func (s *seeder) writeJob(ctx context.Context, j *job) error {
	r := j.run
	ref := "refs/heads/" + r.branch
	if r.pr > 0 {
		ref = fmt.Sprintf("refs/pull/%d/merge", r.pr)
	}
	key := scaleSetKey{pool: j.kind.pool}
	if r.repo.personal() {
		key.repo = r.repo.name // a personal account has a scale set in each repository
	}
	scaleSetID := s.scaleSets[key]
	row, err := s.st.UpsertJob(ctx, store.JobMessage{
		RunnerRequestID: j.requestID,
		State:           j.state(),
		Result:          j.result,
		ScaleSetID:      scaleSetID,
		PoolID:          s.pools[j.kind.pool].ID,
		Repository:      r.repo.name,
		WorkflowRef:     r.repo.name + "/.github/workflows/" + r.wf.file + "@" + ref,
		JobName:         j.kind.name,
		RunID:           r.id,
		Event:           r.event,
		Labels:          []string{j.kind.pool},
		QueuedAt:        j.queued,
		AssignedAt:      j.assigned,
		StartedAt:       j.started,
		FinishedAt:      j.finished,
	})
	if err != nil {
		return err
	}
	j.id = row.ID
	err = s.st.SetJobEnrichment(ctx, j.id, store.JobEnrichment{
		GitHubJobID: j.githubID, WorkflowName: r.wf.name, RunNumber: r.number, RunAttempt: 1,
		HeadBranch: r.branch, HeadSHA: r.sha, CommitMessage: r.message, Event: r.event, Actor: r.actor,
		PRNumber: r.pr, HTMLURL: fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", r.repo.name, r.id, j.githubID),
		Steps: j.steps,
	})
	if err != nil {
		return err
	}
	if b := j.sandbox; b != nil {
		b.JobID, b.ScaleSetID = j.id, scaleSetID
		if err := s.st.InsertSandbox(ctx, *b); err != nil {
			return err
		}
		return s.st.SetJobSandbox(ctx, j.id, b.ID, b.ServerID)
	}
	return nil
}
