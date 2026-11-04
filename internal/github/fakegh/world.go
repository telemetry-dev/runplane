package fakegh

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"hash/crc32"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

type JobState string

const (
	JobQueued    JobState = "queued"    // no scale set has acquired it
	JobAcquired  JobState = "acquired"  // a scale set acquired it; it waits for a runner
	JobRunning   JobState = "running"   // a runner runs it
	JobCompleted JobState = "completed" // Result is set
)

// Job results, as JobCompleted messages give them.
const (
	ResultSucceeded = "succeeded"
	ResultFailed    = "failed"
	ResultCanceled  = "canceled"
)

// JobSpec describes a job to queue. Only Repo and Labels are required.
type JobSpec struct {
	Repo          string   `json:"repo"`                    // "owner/name"
	WorkflowFile  string   `json:"workflow_file,omitempty"` // default "ci.yml"
	Workflow      string   `json:"workflow,omitempty"`      // workflow name, default "CI"
	Name          string   `json:"name,omitempty"`          // job name, default "build"
	Labels        []string `json:"labels"`                  // runs-on
	Steps         []Step   `json:"steps,omitempty"`         // default: one step that prints a line
	Event         string   `json:"event,omitempty"`         // default "push", or "pull_request" with PullRequest
	Branch        string   `json:"branch,omitempty"`        // default "main"
	Actor         string   `json:"actor,omitempty"`         // default "octocat"
	CommitMessage string   `json:"commit_message,omitempty"`
	PullRequest   int      `json:"pull_request,omitempty"` // pull request number
	Fork          bool     `json:"fork,omitempty"`         // the pull request comes from a fork
}

// Step is one step that the fake runner runs.
type Step struct {
	Name  string   `json:"name"`
	Lines []string `json:"lines,omitempty"` // output
	Sleep float64  `json:"sleep,omitempty"` // seconds
	Fail  bool     `json:"fail,omitempty"`  // the step fails, and so the job
	// Run is a bash command that the fake runner runs after it prints Lines.
	// Its output goes to the live step log; an exit code other than 0 fails
	// the step. The stored log from GitHub has only Lines.
	Run string `json:"run,omitempty"`
}

// Job is a snapshot of a job.
type Job struct {
	RequestID  int64     `json:"request_id"` // runner request ID
	ID         int64     `json:"id"`         // REST job ID
	RunID      int64     `json:"run_id"`
	RunAttempt int       `json:"run_attempt"`
	Repo       string    `json:"repo"`
	Name       string    `json:"name"`
	Labels     []string  `json:"labels"`
	State      JobState  `json:"state"`
	Result     string    `json:"result,omitempty"`
	ScaleSetID int       `json:"scale_set_id,omitempty"`
	RunnerID   int64     `json:"runner_id,omitempty"`
	RunnerName string    `json:"runner_name,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Runner is a snapshot of a runner that a JIT config registered.
type Runner struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	ScaleSetID   int    `json:"scale_set_id"`
	Online       bool   `json:"online"`
	JobRequestID int64  `json:"job_request_id,omitempty"` // the job that it runs
}

// ScaleSet is a snapshot of a runner scale set.
type ScaleSet struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Scope  string   `json:"scope"` // "acme" or "acme/api"
	Labels []string `json:"labels"`
	// Session reports if a listener has a message session.
	Session bool `json:"session"`
	// MaxCapacity is the value of the last poll, -1 before the first poll.
	MaxCapacity int `json:"max_capacity"`
	// Unacked counts the messages that the listener has not deleted.
	Unacked int `json:"unacked"`
}

// QueueJob queues a job in a new workflow run and returns its runner
// request ID. The scale sets that match the repository and all labels get
// a JobAvailable message.
func (s *Server) QueueJob(repo, workflowFile, jobName string, labels []string) int64 {
	return s.Queue(JobSpec{Repo: repo, WorkflowFile: workflowFile, Name: jobName, Labels: labels})
}

// Queue is QueueJob with all job fields.
func (s *Server) Queue(spec JobSpec) int64 {
	spec = withDefaults(spec)
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.repos[strings.ToLower(spec.Repo)]
	if rp == nil {
		rp = s.addRepo(s.installFor(spec.Repo), Repo{Name: spec.Repo})
	}
	key := strings.ToLower(spec.Repo) + "/" + spec.WorkflowFile
	s.runSeq[key]++
	s.last.run++
	sha := make([]byte, 20)
	rand.Read(sha)
	rn := &run{id: s.last.run, number: s.runSeq[key], repo: rp, spec: spec, sha: hex.EncodeToString(sha), created: time.Now()}
	s.runs[rn.id] = rn
	return s.newJob(rn).requestID
}

// CancelJob cancels a job. A running job ends after its current step. It
// reports false if the job does not exist or is complete.
func (s *Server) CancelJob(requestID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.job(requestID)
	if j == nil {
		return false
	}
	switch j.state {
	case JobQueued, JobAcquired:
		s.complete(j, ResultCanceled)
	case JobRunning:
		j.cancel = true
		s.notify()
	default:
		return false
	}
	return true
}

// Jobs returns all jobs in request ID order.
func (s *Server) Jobs() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		v := Job{
			RequestID:  j.requestID,
			ID:         j.id,
			RunID:      j.run.id,
			RunAttempt: j.attempt,
			Repo:       j.run.repo.name,
			Name:       j.run.spec.Name,
			Labels:     slices.Clone(j.run.spec.Labels),
			State:      j.state,
			Result:     j.result,
			RunnerID:   j.runnerID,
			RunnerName: j.runnerName,
			QueuedAt:   j.queued,
			StartedAt:  j.started,
			FinishedAt: j.finished,
		}
		if j.set != nil {
			v.ScaleSetID = j.set.id
		}
		out = append(out, v)
	}
	return out
}

// Runners returns the registered runners in ID order. A JIT runner is
// gone after its job.
func (s *Server) Runners() []Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Runner
	for _, rn := range s.runnerList() {
		v := Runner{ID: rn.id, Name: rn.name, ScaleSetID: rn.set.id, Online: rn.online}
		if rn.job != nil {
			v.JobRequestID = rn.job.requestID
		}
		out = append(out, v)
	}
	return out
}

// ScaleSets returns the scale sets in ID order.
func (s *Server) ScaleSets() []ScaleSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScaleSet
	for _, set := range s.setList() {
		out = append(out, ScaleSet{
			ID:          set.id,
			Name:        set.name,
			Scope:       set.scope,
			Labels:      slices.Clone(set.labels),
			Session:     set.session != nil,
			MaxCapacity: set.maxCapacity,
			Unacked:     len(set.queue),
		})
	}
	return out
}

func withDefaults(spec JobSpec) JobSpec {
	spec.WorkflowFile = cmp.Or(spec.WorkflowFile, "ci.yml")
	spec.Workflow = cmp.Or(spec.Workflow, "CI")
	spec.Name = cmp.Or(spec.Name, "build")
	spec.Branch = cmp.Or(spec.Branch, "main")
	spec.Actor = cmp.Or(spec.Actor, "octocat")
	spec.CommitMessage = cmp.Or(spec.CommitMessage, "Update README.md")
	if spec.PullRequest > 0 {
		spec.Event = cmp.Or(spec.Event, "pull_request")
	}
	spec.Event = cmp.Or(spec.Event, "push")
	if len(spec.Steps) == 0 {
		spec.Steps = []Step{{Name: "Run echo hello", Lines: []string{"hello"}}}
	}
	return spec
}

type app struct {
	name, slug string
	setupURL   string // from the manifest
}

type installation struct {
	Installation
	repos []*repo
}

func (inst *installation) selection() string {
	if inst.Selected {
		return "selected"
	}
	return "all"
}

type repo struct {
	id      int64
	name    string // "owner/name"
	private bool
	install *installation // nil: the app cannot read it
}

func (r *repo) owner() string {
	owner, _, _ := strings.Cut(r.name, "/")
	return owner
}

// addRepo adds a repository to inst, which can be nil. The caller holds s.mu.
func (s *Server) addRepo(inst *installation, r Repo) *repo {
	s.last.repo++
	rp := &repo{id: s.last.repo, name: r.Name, private: r.Private, install: inst}
	s.repos[strings.ToLower(r.Name)] = rp
	if inst != nil {
		inst.repos = append(inst.repos, rp)
	}
	return rp
}

// installFor returns the installation on the owner of a repository, or nil.
func (s *Server) installFor(repoName string) *installation {
	owner, _, _ := strings.Cut(repoName, "/")
	for _, inst := range s.installs {
		if strings.EqualFold(inst.Account, owner) {
			return inst
		}
	}
	return nil
}

func (s *Server) install(id int64) *installation {
	for _, inst := range s.installs {
		if inst.ID == id {
			return inst
		}
	}
	return nil
}

// accountID gives each login a stable ID.
func accountID(login string) int64 {
	return int64(crc32.ChecksumIEEE([]byte(strings.ToLower(login))))
}

type run struct {
	id, number int64
	repo       *repo
	spec       JobSpec
	sha        string
	created    time.Time
	jobs       []*job // one for each attempt
}

func (r *run) ref() string {
	if r.spec.PullRequest > 0 {
		return "refs/pull/" + itoa(int64(r.spec.PullRequest)) + "/merge"
	}
	return "refs/heads/" + r.spec.Branch
}

func (r *run) latest() *job { return r.jobs[len(r.jobs)-1] }

type job struct {
	requestID int64
	id        int64
	uuid      string // the jobId of the scale set messages
	run       *run
	attempt   int
	state     JobState
	result    string
	offered   map[int]bool // scale sets that got JobAvailable
	set       *scaleSet    // the scale set that acquired it
	runner    *runner      // while running
	runnerID  int64
	// runnerName stays after the JIT runner is gone.
	runnerName string
	cancel     bool // canceled while running
	queued     time.Time
	acquired   time.Time
	started    time.Time
	finished   time.Time
	log        []byte
}

// newJob queues a new attempt of rn. The caller holds s.mu.
func (s *Server) newJob(rn *run) *job {
	s.last.request++
	s.last.job++
	j := &job{
		requestID: s.last.request,
		id:        s.last.job,
		uuid:      uuid.NewString(),
		run:       rn,
		attempt:   len(rn.jobs) + 1,
		state:     JobQueued,
		offered:   map[int]bool{},
		queued:    time.Now(),
	}
	rn.jobs = append(rn.jobs, j)
	s.jobs = append(s.jobs, j)
	for _, set := range s.setList() {
		s.offer(set, j)
	}
	s.notify()
	return j
}

func (s *Server) job(requestID int64) *job {
	i, ok := slices.BinarySearchFunc(s.jobs, requestID, func(j *job, id int64) int { return cmp.Compare(j.requestID, id) })
	if !ok {
		return nil
	}
	return s.jobs[i]
}

func (s *Server) jobByID(id int64) *job {
	for _, j := range s.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

// offer sends JobAvailable for j to set if set can run it.
func (s *Server) offer(set *scaleSet, j *job) {
	if j.state != JobQueued || j.offered[set.id] || !set.matches(j) {
		return
	}
	j.offered[set.id] = true
	set.pending = append(set.pending, jobMessage{scaleset.MessageTypeJobAvailable, j})
}

// acquire gives j to set and reports if set has it.
func (s *Server) acquire(set *scaleSet, j *job) bool {
	if j.set == set && j.state != JobCompleted {
		return true
	}
	if j.state != JobQueued || !j.offered[set.id] {
		return false
	}
	j.state, j.set, j.acquired = JobAcquired, set, time.Now()
	set.pending = append(set.pending, jobMessage{scaleset.MessageTypeJobAssigned, j})
	return true
}

// assign returns the job of rn: the job that it runs, or else the oldest
// job that its scale set acquired.
func (s *Server) assign(rn *runner) *job {
	if rn.job != nil {
		return rn.job
	}
	for _, j := range s.jobs {
		if j.state == JobAcquired && j.set == rn.set {
			j.state, j.runner, j.runnerID, j.runnerName, j.started = JobRunning, rn, rn.id, rn.name, time.Now()
			rn.job = j
			rn.set.pending = append(rn.set.pending, jobMessage{scaleset.MessageTypeJobStarted, j})
			s.notify()
			return j
		}
	}
	return nil
}

// complete ends j. A JIT runner is gone after its job.
func (s *Server) complete(j *job, result string) {
	if j.cancel {
		result = ResultCanceled
	}
	j.state, j.result, j.finished = JobCompleted, result, time.Now()
	if j.set != nil {
		j.set.pending = append(j.set.pending, jobMessage{scaleset.MessageTypeJobCompleted, j})
	}
	if rn := j.runner; rn != nil {
		j.log = jobLog(j, s.cfg.RunnerVersion)
		j.runner = nil
		delete(s.runners, rn.id)
	}
	s.notify()
}

type scaleSet struct {
	id            int
	scope         string // lower case: "acme" or "acme/api"
	name          string
	labels        []string
	disableUpdate bool
	created       time.Time
	session       *session
	maxCapacity   int
	pending       []jobMessage // events that no message has yet
	queue         []*message   // messages that the listener has not deleted
	sent          []jobMessage // events of all messages, for Redeliver
	lastMessage   int
}

func (set *scaleSet) matches(j *job) bool {
	repo := strings.ToLower(j.run.repo.name)
	if set.scope != repo && set.scope != strings.ToLower(j.run.repo.owner()) {
		return false
	}
	labels := j.run.spec.Labels
	for _, l := range labels {
		if !slices.ContainsFunc(set.labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			return false
		}
	}
	return len(labels) > 0
}

func (s *Server) setList() []*scaleSet {
	var out []*scaleSet
	for _, id := range slices.Sorted(maps.Keys(s.sets)) {
		out = append(out, s.sets[id])
	}
	return out
}

func (s *Server) runnerList() []*runner {
	var out []*runner
	for _, id := range slices.Sorted(maps.Keys(s.runners)) {
		out = append(out, s.runners[id])
	}
	return out
}

// stats counts the jobs and runners of set.
func (s *Server) stats(set *scaleSet) *scaleset.RunnerScaleSetStatistic {
	st := &scaleset.RunnerScaleSetStatistic{}
	for _, j := range s.jobs {
		switch {
		case j.state == JobQueued && j.offered[set.id]:
			st.TotalAvailableJobs++
		case j.set == set && j.state == JobAcquired:
			st.TotalAcquiredJobs++
			st.TotalAssignedJobs++
		case j.set == set && j.state == JobRunning:
			st.TotalAssignedJobs++
			st.TotalRunningJobs++
		}
	}
	for _, rn := range s.runners {
		if rn.set == set {
			st.TotalRegisteredRunners++
			if rn.job != nil {
				st.TotalBusyRunners++
			}
		}
	}
	st.TotalIdleRunners = st.TotalRegisteredRunners - st.TotalBusyRunners
	return st
}

type jobMessage struct {
	kind scaleset.MessageType
	job  *job
}

type message struct {
	id   int
	jobs []jobMessage
}

// nextMessage returns the first message after last. If the queue has none,
// it makes one from the pending events. As on GitHub, JobAvailable waits
// while the scale set has maxCapacity assigned jobs; a negative maxCapacity
// has no limit.
func (s *Server) nextMessage(set *scaleSet, last, maxCapacity int) *message {
	for _, m := range set.queue {
		if m.id > last {
			return m
		}
	}
	room := maxCapacity - s.stats(set).TotalAssignedJobs
	var take, keep []jobMessage
	for _, e := range set.pending {
		if e.kind == scaleset.MessageTypeJobAvailable {
			if e.job.state != JobQueued {
				continue // acquired by another scale set or canceled
			}
			if maxCapacity >= 0 && room <= 0 {
				keep = append(keep, e)
				continue
			}
			room--
		}
		take = append(take, e)
	}
	set.pending = keep
	if len(take) == 0 {
		return nil
	}
	set.sent = append(set.sent, take...)
	set.lastMessage++
	m := &message{id: set.lastMessage, jobs: take}
	set.queue = append(set.queue, m)
	return m
}

// BreakJIT makes the JIT configs of the scale sets with this name not
// valid, so that their runners exit before they connect to GitHub.
func (s *Server) BreakJIT(scaleSet string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken = append(s.broken, scaleSet)
}

// Redeliver sends all job events that the scale set got so far once more,
// in one new message, as GitHub can when a delete or an acquire is lost. It
// returns false if the scale set does not exist or got no events.
func (s *Server) Redeliver(scaleSetID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.sets[scaleSetID]
	if set == nil || len(set.sent) == 0 {
		return false
	}
	set.lastMessage++
	set.queue = append(set.queue, &message{id: set.lastMessage, jobs: slices.Clone(set.sent)})
	s.notify()
	return true
}

type session struct {
	id    uuid.UUID
	owner string
	set   *scaleSet
	seen  time.Time
	polls int // open polls
}

// live reports if the session blocks a new one.
func (s *Server) live(sess *session) bool {
	return sess.polls > 0 || time.Since(sess.seen) < s.cfg.SessionTimeout
}

type runner struct {
	id     int64
	name   string
	set    *scaleSet
	token  string // from the JIT config
	online bool
	seen   time.Time
	job    *job
}

// watchRunners fails the job of each busy runner that has been silent for
// RunnerTimeout, as GitHub does when a runner dies.
func (s *Server) watchRunners() {
	t := time.NewTicker(s.cfg.RunnerTimeout / 4)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
		}
		s.mu.Lock()
		for _, rn := range s.runners {
			if rn.job != nil && time.Since(rn.seen) > s.cfg.RunnerTimeout {
				s.log.Info("runner lost", "runner", rn.name, "job", rn.job.run.spec.Name)
				s.complete(rn.job, ResultFailed)
			}
		}
		s.mu.Unlock()
	}
}
