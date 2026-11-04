package fakegh

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// The Actions service: runner groups, scale sets, message sessions, and
// runners. The server gives its own base URL as the service URL. Each call
// has the admin token, whose scope (organization or repository) comes from
// the config URL of the client.

const defaultGroupID = 1

func (s *Server) actionsRoutes() {
	m := s.mux
	m.HandleFunc("GET /_apis/runtime/runnergroups/{$}", s.listGroups)
	m.HandleFunc("GET /_apis/runtime/runnerscalesets", s.listScaleSets)
	m.HandleFunc("POST /_apis/runtime/runnerscalesets", s.createScaleSet)
	m.HandleFunc("GET /_apis/runtime/runnerscalesets/{id}", s.getScaleSet)
	m.HandleFunc("PATCH /_apis/runtime/runnerscalesets/{id}", s.updateScaleSet)
	m.HandleFunc("DELETE /_apis/runtime/runnerscalesets/{id}", s.deleteScaleSet)
	m.HandleFunc("POST /_apis/runtime/runnerscalesets/{id}/sessions", s.createSession)
	m.HandleFunc("PATCH /_apis/runtime/runnerscalesets/{id}/sessions/{session}", s.refreshSession)
	m.HandleFunc("DELETE /_apis/runtime/runnerscalesets/{id}/sessions/{session}", s.deleteSession)
	m.HandleFunc("POST /_apis/runtime/runnerscalesets/{id}/acquirejobs", s.acquireJobs)
	m.HandleFunc("POST /_apis/runtime/runnerscalesets/{id}/generatejitconfig", s.generateJIT)
	m.HandleFunc("GET /_apis/distributedtask/pools/{pool}/agents", s.findRunner)
	m.HandleFunc("GET /_apis/distributedtask/pools/{pool}/agents/{id}", s.getRunner)
	m.HandleFunc("DELETE /_apis/distributedtask/pools/{pool}/agents/{id}", s.removeRunner)
	m.HandleFunc("GET /_broker/sessions/{session}/messages", s.getMessage)
	m.HandleFunc("DELETE /_broker/sessions/{session}/messages/{message}", s.deleteMessage)
}

// adminScope returns the scope of the admin token, or writes 401.
func (s *Server) adminScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	t := s.auth(r, adminToken)
	if t == nil {
		actionsError(w, http.StatusUnauthorized, "VssUnauthorizedException", "The admin token is missing, wrong, or expired.")
		return "", false
	}
	return t.scope, true
}

// scaleSetIn returns the scale set of the path in scope, or writes 404.
func (s *Server) scaleSetIn(w http.ResponseWriter, r *http.Request, scope string) *scaleSet {
	id, _ := strconv.Atoi(r.PathValue("id"))
	set := s.sets[id]
	if set == nil || set.scope != scope {
		actionsError(w, http.StatusNotFound, "RunnerScaleSetNotFoundException", fmt.Sprintf("Runner scale set %s not found.", r.PathValue("id")))
		return nil
	}
	return set
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	groups := []scaleset.RunnerGroup{}
	if name := r.URL.Query().Get("groupName"); name == "" || strings.EqualFold(name, "Default") {
		size := 0
		for _, rn := range s.runners {
			if rn.set.scope == scope {
				size++
			}
		}
		groups = append(groups, scaleset.RunnerGroup{ID: defaultGroupID, Name: "Default", Size: size, IsDefault: true})
	}
	writeJSON(w, http.StatusOK, scaleset.RunnerGroupList{Count: len(groups), RunnerGroups: groups})
}

func (s *Server) listScaleSets(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	sets := []scaleset.RunnerScaleSet{}
	for _, set := range s.setList() {
		if set.scope != scope || (q.Has("name") && !strings.EqualFold(set.name, q.Get("name"))) {
			continue
		}
		if g := q.Get("runnerGroupId"); g != "" && g != strconv.Itoa(defaultGroupID) {
			continue
		}
		sets = append(sets, s.scaleSetJSON(r, set))
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(sets), "value": sets})
}

func (s *Server) createScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	var in scaleset.RunnerScaleSet
	if err := readJSON(r, &in); err != nil || in.Name == "" {
		actionsError(w, http.StatusBadRequest, "ArgumentException", "The scale set needs a name.")
		return
	}
	if !s.checkScaleSet(w, scope, 0, in) {
		return
	}
	s.last.set++
	set := &scaleSet{id: s.last.set, scope: scope, name: in.Name, created: time.Now(), maxCapacity: -1}
	set.labels, set.disableUpdate = labelNames(in), in.RunnerSetting.DisableUpdate
	s.sets[set.id] = set
	// GitHub gives queued jobs to a new matching scale set.
	for _, j := range s.jobs {
		s.offer(set, j)
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.scaleSetJSON(r, set))
}

func (s *Server) getScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	if set := s.scaleSetIn(w, r, scope); set != nil {
		writeJSON(w, http.StatusOK, s.scaleSetJSON(r, set))
	}
}

func (s *Server) updateScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	set := s.scaleSetIn(w, r, scope)
	if set == nil {
		return
	}
	var in scaleset.RunnerScaleSet
	if err := readJSON(r, &in); err != nil {
		actionsError(w, http.StatusBadRequest, "ArgumentException", err.Error())
		return
	}
	if !s.checkScaleSet(w, scope, set.id, in) {
		return
	}
	if in.Name != "" {
		set.name = in.Name
	}
	if len(in.Labels) > 0 {
		set.labels = labelNames(in)
	}
	set.disableUpdate = in.RunnerSetting.DisableUpdate
	for _, j := range s.jobs {
		s.offer(set, j)
	}
	s.notify()
	writeJSON(w, http.StatusOK, s.scaleSetJSON(r, set))
}

// checkScaleSet checks the runner group and that the name is free in the
// scope, or writes the error.
func (s *Server) checkScaleSet(w http.ResponseWriter, scope string, id int, in scaleset.RunnerScaleSet) bool {
	if in.RunnerGroupID != 0 && in.RunnerGroupID != defaultGroupID {
		actionsError(w, http.StatusNotFound, "RunnerGroupNotFoundException", fmt.Sprintf("Runner group %d not found.", in.RunnerGroupID))
		return false
	}
	for _, set := range s.sets {
		if set.id != id && set.scope == scope && strings.EqualFold(set.name, in.Name) {
			actionsError(w, http.StatusConflict, "RunnerScaleSetExistsException", fmt.Sprintf("A runner scale set with the name %s already exists.", in.Name))
			return false
		}
	}
	return true
}

// deleteScaleSet removes the scale set, its sessions, and its idle runners.
// Busy runners finish their jobs. Jobs that wait for a runner go back to
// the queue.
func (s *Server) deleteScaleSet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	set := s.scaleSetIn(w, r, scope)
	if set == nil {
		return
	}
	delete(s.sets, set.id)
	if set.session != nil {
		delete(s.sessions, set.session.id)
	}
	for id, rn := range s.runners {
		if rn.set == set && rn.job == nil {
			delete(s.runners, id)
		}
	}
	for _, j := range s.jobs {
		delete(j.offered, set.id)
		if j.set == set && j.state == JobAcquired {
			j.state, j.set = JobQueued, nil
			for _, other := range s.setList() {
				s.offer(other, j)
			}
		}
	}
	s.notify()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scaleSetJSON(r *http.Request, set *scaleSet) scaleset.RunnerScaleSet {
	out := scaleset.RunnerScaleSet{
		ID:                 set.id,
		Name:               set.name,
		RunnerGroupID:      defaultGroupID,
		RunnerGroupName:    "Default",
		RunnerSetting:      scaleset.RunnerSetting{DisableUpdate: set.disableUpdate},
		CreatedOn:          set.created,
		RunnerJitConfigURL: fmt.Sprintf("%s/_apis/runtime/runnerscalesets/%d/generatejitconfig", baseURL(r), set.id),
		Statistics:         s.stats(set),
	}
	for _, l := range set.labels {
		out.Labels = append(out.Labels, scaleset.Label{Type: "System", Name: l})
	}
	return out
}

func labelNames(in scaleset.RunnerScaleSet) []string {
	if len(in.Labels) == 0 {
		return []string{in.Name}
	}
	var names []string
	for _, l := range in.Labels {
		names = append(names, l.Name)
	}
	return names
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	set := s.scaleSetIn(w, r, scope)
	if set == nil {
		return
	}
	var in scaleset.RunnerScaleSetSession
	readJSON(r, &in)
	if old := set.session; old != nil {
		if s.live(old) {
			actionsError(w, http.StatusConflict, "TaskAgentSessionConflictException",
				fmt.Sprintf("The runner scale set %s already has an active session for owner %s.", set.name, old.owner))
			return
		}
		delete(s.sessions, old.id)
	}
	sess := &session{id: uuid.New(), owner: in.OwnerName, set: set, seen: time.Now()}
	s.sessions[sess.id] = sess
	set.session = sess
	s.notify()
	writeJSON(w, http.StatusOK, s.sessionJSON(r, sess))
}

func (s *Server) refreshSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessionIn(w, r); sess != nil {
		sess.seen = time.Now()
		writeJSON(w, http.StatusOK, s.sessionJSON(r, sess))
	}
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessionIn(w, r); sess != nil {
		delete(s.sessions, sess.id)
		sess.set.session = nil
		s.notify()
		w.WriteHeader(http.StatusNoContent)
	}
}

// sessionIn returns the session of the path, or writes the error.
func (s *Server) sessionIn(w http.ResponseWriter, r *http.Request) *session {
	scope, ok := s.adminScope(w, r)
	if !ok {
		return nil
	}
	set := s.scaleSetIn(w, r, scope)
	if set == nil {
		return nil
	}
	id, _ := uuid.Parse(r.PathValue("session"))
	sess := s.sessions[id]
	if sess == nil || sess.set != set {
		actionsError(w, http.StatusNotFound, "TaskAgentSessionExpiredException", "The message session does not exist.")
		return nil
	}
	return sess
}

// sessionJSON returns the session with a new message queue token.
func (s *Server) sessionJSON(r *http.Request, sess *session) scaleset.RunnerScaleSetSession {
	set := s.scaleSetJSON(r, sess.set)
	return scaleset.RunnerScaleSetSession{
		SessionID:               sess.id,
		OwnerName:               sess.owner,
		RunnerScaleSet:          &set,
		MessageQueueURL:         fmt.Sprintf("%s/_broker/sessions/%s/messages", baseURL(r), sess.id),
		MessageQueueAccessToken: s.issue(&token{kind: queueToken, session: sess}),
		Statistics:              set.Statistics,
	}
}

// queueSession returns the session of the message queue token, which must
// be live and match the session or scale set of the path. Else it writes
// 401, as for an expired token.
func (s *Server) queueSession(w http.ResponseWriter, r *http.Request) *session {
	t := s.auth(r, queueToken)
	ok := t != nil && s.sessions[t.session.id] == t.session
	if v := r.PathValue("session"); ok && v != "" {
		ok = v == t.session.id.String()
	}
	if v := r.PathValue("id"); ok && v != "" {
		ok = v == strconv.Itoa(t.session.set.id)
	}
	if !ok {
		actionsError(w, http.StatusUnauthorized, "TaskAgentSessionExpiredException", "The message queue token is wrong or expired.")
		return nil
	}
	return t.session
}

// getMessage is the long poll of the listener. It returns 202 with no body
// when no message comes within PollTimeout.
func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	last, _ := strconv.Atoi(r.URL.Query().Get("lastMessageId"))
	maxCapacity := -1
	if v := r.Header.Get(scaleset.HeaderScaleSetMaxCapacity); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			actionsError(w, http.StatusBadRequest, "ArgumentException", "Bad "+scaleset.HeaderScaleSetMaxCapacity+" header.")
			return
		}
		maxCapacity = n
	}

	s.mu.Lock()
	sess := s.queueSession(w, r)
	if sess == nil {
		s.mu.Unlock()
		return
	}
	sess.polls++
	sess.seen = time.Now()
	sess.set.maxCapacity = maxCapacity
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		sess.polls--
		sess.seen = time.Now()
		s.mu.Unlock()
	}()

	timeout := time.NewTimer(s.cfg.PollTimeout)
	defer timeout.Stop()
	for {
		s.mu.Lock()
		if s.sessions[sess.id] != sess {
			s.mu.Unlock()
			actionsError(w, http.StatusNotFound, "TaskAgentSessionExpiredException", "The message session was deleted.")
			return
		}
		var body any
		if m := s.nextMessage(sess.set, last, maxCapacity); m != nil {
			body = s.messageJSON(r, sess.set, m)
		}
		wake := s.wake
		s.mu.Unlock()
		if body != nil {
			writeJSON(w, http.StatusOK, body)
			return
		}
		select {
		case <-wake:
		case <-timeout.C:
			w.WriteHeader(http.StatusAccepted)
			return
		case <-s.closed:
			w.WriteHeader(http.StatusAccepted)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) deleteMessage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.queueSession(w, r)
	if sess == nil {
		return
	}
	id, _ := strconv.Atoi(r.PathValue("message"))
	set := sess.set
	set.queue = slices.DeleteFunc(set.queue, func(m *message) bool { return m.id == id })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) messageJSON(r *http.Request, set *scaleSet, m *message) any {
	var body []any
	for _, e := range m.jobs {
		j := e.job
		owner, name, _ := strings.Cut(j.run.repo.name, "/")
		base := scaleset.JobMessageBase{
			JobMessageType:     scaleset.JobMessageType{MessageType: e.kind},
			RunnerRequestID:    j.requestID,
			RepositoryName:     name,
			OwnerName:          owner,
			JobID:              j.uuid,
			JobWorkflowRef:     j.run.repo.name + "/.github/workflows/" + j.run.spec.WorkflowFile + "@" + j.run.ref(),
			JobDisplayName:     j.run.spec.Name,
			WorkflowRunID:      j.run.id,
			EventName:          j.run.spec.Event,
			RequestLabels:      j.run.spec.Labels,
			QueueTime:          j.queued,
			ScaleSetAssignTime: j.acquired,
			RunnerAssignTime:   j.started,
			FinishTime:         j.finished,
		}
		switch e.kind {
		case scaleset.MessageTypeJobAvailable:
			url := fmt.Sprintf("%s/_apis/runtime/runnerscalesets/%d/acquirejobs", baseURL(r), set.id)
			body = append(body, scaleset.JobAvailable{AcquireJobURL: url, JobMessageBase: base})
		case scaleset.MessageTypeJobAssigned:
			body = append(body, scaleset.JobAssigned{JobMessageBase: base})
		case scaleset.MessageTypeJobStarted:
			body = append(body, scaleset.JobStarted{RunnerID: int(j.runnerID), RunnerName: j.runnerName, JobMessageBase: base})
		case scaleset.MessageTypeJobCompleted:
			body = append(body, scaleset.JobCompleted{Result: j.result, RunnerID: int(j.runnerID), RunnerName: j.runnerName, JobMessageBase: base})
		}
	}
	// The message body is a JSON string that holds the job messages.
	text, _ := json.Marshal(body)
	return map[string]any{
		"messageId":   m.id,
		"messageType": "RunnerScaleSetJobMessages",
		"body":        string(text),
		"statistics":  s.stats(set),
	}
}

// acquireJobs gives the requested jobs to the scale set and returns the IDs
// that it got. The listener calls it with the message queue token.
func (s *Server) acquireJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.queueSession(w, r)
	if sess == nil {
		return
	}
	var ids []int64
	if err := readJSON(r, &ids); err != nil {
		actionsError(w, http.StatusBadRequest, "ArgumentException", err.Error())
		return
	}
	got := []int64{}
	for _, id := range ids {
		if j := s.job(id); j != nil && s.acquire(sess.set, j) {
			got = append(got, id)
		}
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{"count": len(got), "value": got})
}

func (s *Server) generateJIT(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	set := s.scaleSetIn(w, r, scope)
	if set == nil {
		return
	}
	var in scaleset.RunnerScaleSetJitRunnerSetting
	if err := readJSON(r, &in); err != nil || in.Name == "" {
		actionsError(w, http.StatusBadRequest, "ArgumentException", "The runner needs a name.")
		return
	}
	for _, rn := range s.runners {
		if rn.set.scope == scope && strings.EqualFold(rn.name, in.Name) {
			actionsError(w, http.StatusConflict, "AgentExistsException", fmt.Sprintf("A runner with the name %s already exists.", in.Name))
			return
		}
	}
	s.last.runner++
	rn := &runner{id: s.last.runner, name: in.Name, set: set, token: rand.Text(), seen: time.Now()}
	s.runners[rn.id] = rn
	s.notify()
	config := jitConfig(cmp.Or(s.cfg.RunnerURL, baseURL(r)), rn, in.WorkFolder)
	if slices.Contains(s.broken, set.name) {
		config = "not a JIT config"
	}
	writeJSON(w, http.StatusOK, scaleset.RunnerScaleSetJitRunnerConfig{Runner: runnerRef(rn), EncodedJITConfig: config})
}

func runnerRef(rn *runner) *scaleset.RunnerReference {
	return &scaleset.RunnerReference{ID: int(rn.id), Name: rn.name, RunnerScaleSetID: rn.set.id}
}

func (s *Server) findRunner(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	refs := []scaleset.RunnerReference{}
	name := r.URL.Query().Get("agentName")
	for _, rn := range s.runners {
		if rn.set.scope == scope && strings.EqualFold(rn.name, name) {
			refs = append(refs, *runnerRef(rn))
		}
	}
	writeJSON(w, http.StatusOK, scaleset.RunnerReferenceList{Count: len(refs), RunnerReferences: refs})
}

// runnerIn returns the runner of the path in scope, or writes 404.
func (s *Server) runnerIn(w http.ResponseWriter, r *http.Request, scope string) *runner {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rn := s.runners[id]
	if rn == nil || rn.set.scope != scope {
		actionsError(w, http.StatusNotFound, "TaskAgentNotFoundException", fmt.Sprintf("Runner %s not found.", r.PathValue("id")))
		return nil
	}
	return rn
}

func (s *Server) getRunner(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	if rn := s.runnerIn(w, r, scope); rn != nil {
		writeJSON(w, http.StatusOK, runnerRef(rn))
	}
}

// removeRunner removes an idle runner. A busy runner gets 400 with
// JobStillRunningException.
func (s *Server) removeRunner(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, ok := s.adminScope(w, r)
	if !ok {
		return
	}
	rn := s.runnerIn(w, r, scope)
	if rn == nil {
		return
	}
	if rn.job != nil {
		actionsError(w, http.StatusBadRequest, "TaskAgentJobStillRunningException", fmt.Sprintf("Runner %s has a running job.", rn.name))
		return
	}
	delete(s.runners, rn.id)
	s.notify()
	w.WriteHeader(http.StatusNoContent)
}
