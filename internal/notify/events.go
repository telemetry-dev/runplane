package notify

import (
	"fmt"
	"strings"
	"time"
)

// Type names an event. Channels store the types they receive.
type Type string

const (
	JobFailed         Type = "job.failed"
	ServerUnreachable Type = "server.unreachable"
	DiskLow           Type = "disk.low"
	RunnerDeprecation Type = "runner.deprecation"
	UpdateAvailable   Type = "update.available"
	BackupFailed      Type = "backup.failed"
	// Test is the event of the "Send test" button. Channels do not subscribe to it.
	Test Type = "test"
)

// Event is one notification.
type Event struct {
	Type  Type
	Title string
	Text  string
	URL   string         // dashboard page of the event; empty if the dashboard URL is not known
	Data  map[string]any // details for webhook receivers
	At    time.Time      // Send sets the current time if zero
}

// Job is the part of a job that a notification shows.
type Job struct {
	ID         string
	Repository string
	Workflow   string
	Name       string
	Branch     string
	Actor      string
	RunNumber  int64
	Duration   time.Duration
	GitHubURL  string
}

// The constructors below take the dashboard base URL ("https://runplane.example.com").
// An empty base gives events without links.

func JobFailedEvent(dashboard string, j Job) Event {
	parts := []string{j.Repository}
	if j.Workflow != "" {
		w := j.Workflow
		if j.RunNumber > 0 {
			w += fmt.Sprintf(" #%d", j.RunNumber)
		}
		parts = append(parts, w)
	}
	if j.Branch != "" {
		parts = append(parts, "branch "+j.Branch)
	}
	if j.Actor != "" {
		parts = append(parts, "by "+j.Actor)
	}
	if j.Duration > 0 {
		parts = append(parts, "after "+j.Duration.Round(time.Second).String())
	}
	return Event{
		Type:  JobFailed,
		Title: "Job failed: " + j.Name,
		Text:  strings.Join(parts, " · "),
		URL:   link(dashboard, "/jobs/"+j.ID),
		Data: map[string]any{
			"job_id":      j.ID,
			"repository":  j.Repository,
			"workflow":    j.Workflow,
			"job_name":    j.Name,
			"branch":      j.Branch,
			"actor":       j.Actor,
			"run_number":  j.RunNumber,
			"duration_ms": j.Duration.Milliseconds(),
			"github_url":  j.GitHubURL,
		},
	}
}

func ServerUnreachableEvent(dashboard, serverID, name string, lastSeen time.Time) Event {
	return Event{
		Type:  ServerUnreachable,
		Title: "Server unreachable: " + name,
		Text:  fmt.Sprintf("runplane has had no connection to %s since %s. Jobs do not start on this server.", name, lastSeen.UTC().Format("2006-01-02 15:04 UTC")),
		URL:   link(dashboard, "/servers/"+serverID),
		Data: map[string]any{
			"server_id":    serverID,
			"server_name":  name,
			"last_seen_at": lastSeen.UTC().Format(time.RFC3339),
		},
	}
}

func DiskLowEvent(dashboard, serverID, name string, freePercent float64, minFreePercent int) Event {
	return Event{
		Type:  DiskLow,
		Title: "Disk almost full on " + name,
		Text:  fmt.Sprintf("%.0f%% of the disk is free. New sandboxes start only when more than %d%% is free.", freePercent, minFreePercent),
		URL:   link(dashboard, "/servers/"+serverID),
		Data: map[string]any{
			"server_id":        serverID,
			"server_name":      name,
			"free_percent":     freePercent,
			"min_free_percent": minFreePercent,
		},
	}
}

func RunnerDeprecationEvent(dashboard, version string, end time.Time) Event {
	date := end.UTC().Format("2006-01-02")
	return Event{
		Type:  RunnerDeprecation,
		Title: fmt.Sprintf("Runner %s stops working on %s", version, date),
		Text:  fmt.Sprintf("GitHub stops sending jobs to runner %s on %s. Make sure that a newer runner version is ready.", version, date),
		URL:   link(dashboard, "/settings/runners"),
		Data:  map[string]any{"version": version, "end_date": date},
	}
}

func UpdateAvailableEvent(dashboard, current, latest, releaseURL string) Event {
	return Event{
		Type:  UpdateAvailable,
		Title: fmt.Sprintf("runplane %s is available", latest),
		Text:  fmt.Sprintf("This server runs %s. You can update in Settings, General.", current),
		URL:   link(dashboard, "/settings/general"),
		Data: map[string]any{
			"current_version": current,
			"latest_version":  latest,
			"release_url":     releaseURL,
		},
	}
}

func BackupFailedEvent(dashboard string, err error) Event {
	return Event{
		Type:  BackupFailed,
		Title: "Database backup failed",
		Text:  err.Error(),
		URL:   link(dashboard, "/settings/general"),
		Data:  map[string]any{"error": err.Error()},
	}
}

func TestEvent(dashboard string) Event {
	return Event{
		Type:  Test,
		Title: "Test notification from runplane",
		Text:  "This channel receives runplane notifications.",
		URL:   link(dashboard, "/settings/notifications"),
	}
}

func link(dashboard, path string) string {
	if dashboard == "" {
		return ""
	}
	return strings.TrimRight(dashboard, "/") + path
}
