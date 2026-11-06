package github

import (
	"cmp"
	"hash/fnv"
	"log/slog"

	"github.com/actions/scaleset"

	"github.com/runplane/runplane/internal/scheduler"
	"github.com/runplane/runplane/internal/store"
)

// message converts a listener message to the scheduler message. It also
// returns the request ID of each offered job by the key of its job row,
// because GitHub acquires a job by its request ID.
func message(log *slog.Logger, msg *scaleset.RunnerScaleSetMessage) (scheduler.Message, map[int64]int64) {
	var m scheduler.Message
	offered := map[int64]int64{}
	if msg == nil {
		return m, offered
	}
	if msg.Statistics != nil {
		st := statsOf(*msg.Statistics)
		m.Stats = &st
	}
	for _, j := range msg.JobAvailableMessages {
		log.Debug("job message", "type", "available", "request", j.RunnerRequestID, "job", j.JobID, "name", j.JobDisplayName)
		jm := jobMessage(store.JobQueued, j.JobMessageBase)
		offered[jm.RunnerRequestID] = j.RunnerRequestID
		m.Available = append(m.Available, jm)
	}
	for _, j := range msg.JobAssignedMessages {
		log.Debug("job message", "type", "assigned", "request", j.RunnerRequestID, "job", j.JobID, "name", j.JobDisplayName)
		m.Assigned = append(m.Assigned, jobMessage(store.JobAssigned, j.JobMessageBase))
	}
	for _, j := range msg.JobStartedMessages {
		log.Debug("job message", "type", "started", "request", j.RunnerRequestID, "job", j.JobID, "name", j.JobDisplayName, "runner", j.RunnerName)
		jm := jobMessage(store.JobRunning, j.JobMessageBase)
		jm.RunnerName = j.RunnerName
		m.Started = append(m.Started, jm)
	}
	for _, j := range msg.JobCompletedMessages {
		log.Debug("job message", "type", "completed", "request", j.RunnerRequestID, "job", j.JobID, "name", j.JobDisplayName, "result", j.Result)
		jm := jobMessage(store.JobCompleted, j.JobMessageBase)
		jm.RunnerName = j.RunnerName
		jm.Result = cmp.Or(store.ResultOf(j.Result), store.ResultFailed)
		m.Completed = append(m.Completed, jm)
	}
	return m, offered
}

func jobMessage(state string, m scaleset.JobMessageBase) scheduler.JobMessage {
	repo := m.RepositoryName
	if m.OwnerName != "" {
		repo = m.OwnerName + "/" + m.RepositoryName
	}
	return scheduler.JobMessage{JobMessage: store.JobMessage{
		RunnerRequestID: jobKey(m),
		State:           state,
		Repository:      repo,
		WorkflowRef:     m.JobWorkflowRef,
		JobName:         m.JobDisplayName,
		RunID:           m.WorkflowRunID,
		Event:           m.EventName,
		Labels:          m.RequestLabels,
		QueuedAt:        m.QueueTime,
		AssignedAt:      m.ScaleSetAssignTime,
		StartedAt:       m.RunnerAssignTime,
		FinishedAt:      m.FinishTime,
	}}
}

// jobKey gives the key of the job row of a message. GitHub sends
// runnerRequestId 0 in scale set messages and names each job by its jobId,
// a UUID. For such a message the key comes from the jobId: a negative
// number, so it cannot match a real request ID.
func jobKey(m scaleset.JobMessageBase) int64 {
	if m.RunnerRequestID != 0 || m.JobID == "" {
		return m.RunnerRequestID
	}
	h := fnv.New64a()
	h.Write([]byte(m.JobID))
	return -int64(h.Sum64()>>1) - 1
}

func statsOf(x scaleset.RunnerScaleSetStatistic) store.ScaleSetStats {
	return store.ScaleSetStats{
		AvailableJobs:     x.TotalAvailableJobs,
		AcquiredJobs:      x.TotalAcquiredJobs,
		AssignedJobs:      x.TotalAssignedJobs,
		RunningJobs:       x.TotalRunningJobs,
		RegisteredRunners: x.TotalRegisteredRunners,
		BusyRunners:       x.TotalBusyRunners,
		IdleRunners:       x.TotalIdleRunners,
	}
}
