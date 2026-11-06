package main

import (
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/github"
	"github.com/runplane/runplane/internal/scheduler"
)

// githubRef breaks the construction cycle of the github service: the
// scheduler and the runner versions service get it first, and the github
// service fills it.
type githubRef struct{ *github.Service }

// schedulerFleet gives the fleet to the scheduler, whose drivers have only
// the methods that the scheduler uses.
type schedulerFleet struct{ *fleet.Fleet }

func (f schedulerFleet) Online() []scheduler.Host {
	var hosts []scheduler.Host
	for _, h := range f.Fleet.Online() {
		hosts = append(hosts, scheduler.Host{ServerID: h.ServerID, Name: h.Name, Arch: h.Arch, Driver: h.Driver})
	}
	return hosts
}

func (f schedulerFleet) Driver(serverID string) scheduler.Driver { return f.Fleet.Driver(serverID) }
