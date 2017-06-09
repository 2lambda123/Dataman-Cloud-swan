package janitor

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

type Upstreams struct {
	Upstreams []*Upstream `json:"upstreams"`
	sync.RWMutex
}

type Upstream struct {
	AppID    string    `json:"app_id"` // uniq id of upstream
	AppAlias string    `json:"app_alias"`
	Targets  []*Target `json:"targets"`
	sessions *Sessions
	balancer Balancer
}

func newUpstream(appID, appAlias string) *Upstream {
	return &Upstream{
		AppID:    appID,
		AppAlias: appAlias,
		Targets:  make([]*Target, 0, 0),
		balancer: &WeightBalancer{}, // default balancer
		sessions: newSessions(),     // sessions store
	}
}

func (us *Upstreams) allUps() []*Upstream {
	us.RLock()
	defer us.RUnlock()
	return us.Upstreams
}

func (us *Upstreams) allSess() map[string]*Sessions {
	us.RLock()
	defer us.RUnlock()

	ret := make(map[string]*Sessions)
	for _, u := range us.Upstreams {
		ret[u.AppID] = u.sessions
	}
	return ret
}

func (us *Upstreams) upsertTarget(target *Target) error {
	us.Lock()
	defer us.Unlock()

	var (
		appID    = target.AppID
		appAlias = target.AppAlias
		taskID   = target.TaskID
	)

	_, u := us.getUpstreamByID(appID)
	// add new upstream
	if u == nil {
		if i, _ := us.getUpstreamByAlias(appAlias); i >= 0 {
			return fmt.Errorf("alias [%s] conflict", appAlias)
		}
		u = newUpstream(appID, appAlias)
		u.Targets = append(u.Targets, target)
		us.Upstreams = append(us.Upstreams, u)
		return nil
	}

	_, t := u.getTarget(taskID)

	// add new target
	if t == nil {
		u.Targets = append(u.Targets, target)
		return nil
	}

	// update target
	t.VersionID = target.VersionID
	t.AppVersion = target.AppVersion
	t.TaskIP = target.TaskIP
	t.TaskPort = target.TaskPort
	t.Weight = target.Weight
	return nil
}

func (us *Upstreams) getTarget(appID, taskID string) *Target {
	us.RLock()
	defer us.RUnlock()

	_, u := us.getUpstreamByID(appID)
	if u == nil {
		return nil
	}

	_, t := u.getTarget(taskID)
	return t
}

func (us *Upstreams) removeTarget(target *Target) {
	us.Lock()
	defer us.Unlock()

	var (
		appID  = target.AppID
		taskID = target.TaskID
	)

	idxu, u := us.getUpstreamByID(appID)
	if u == nil {
		return
	}

	idxt, t := u.getTarget(taskID)
	if t == nil {
		return
	}

	// remove target & session
	u.Targets = append(u.Targets[:idxt], u.Targets[idxt+1:]...)
	u.sessions.remove(taskID)

	// remove empty upstream & stop sessions gc
	if len(u.Targets) == 0 {
		u.sessions.stop()
		us.Upstreams = append(us.Upstreams[:idxu], us.Upstreams[idxu+1:]...)
	}
}

// lookup similar as lookup, but by app alias
func (us *Upstreams) lookupAlias(remoteIP, appAlias string) *Target {
	us.RLock()
	_, u := us.getUpstreamByAlias(appAlias)
	us.RUnlock()

	if u == nil {
		return nil
	}

	appID := u.AppID
	return us.lookup(remoteIP, appID, "")
}

// lookup select a suitable backend according by sessions & balancer
func (us *Upstreams) lookup(remoteIP, appID, taskID string) *Target {
	var (
		u *Upstream
		t *Target
	)

	if _, u = us.getUpstreamByID(appID); u == nil {
		return nil
	}

	defer func() {
		if t != nil {
			u.sessions.update(remoteIP, t)
		}
	}()

	// obtain session
	if t = u.sessions.get(remoteIP); t != nil {
		return t
	}

	// obtain specified task backend
	if taskID != "" {
		t = us.getTarget(appID, taskID)
		return t
	}

	// use balancer to obtain a new backend
	t = us.nextTarget(appID)
	return t
}

func (us *Upstreams) nextTarget(appID string) *Target {
	us.RLock()
	defer us.RUnlock()

	_, u := us.getUpstreamByID(appID)
	if u == nil {
		return nil
	}

	return u.balancer.Next(u.Targets)
}

// note: must be called under protection of mutext lock
func (us *Upstreams) getUpstreamByID(appID string) (int, *Upstream) {
	for i, v := range us.Upstreams {
		if v.AppID == appID {
			return i, v
		}
	}
	return -1, nil
}

// note: must be called under protection of mutext lock
func (us *Upstreams) getUpstreamByAlias(alias string) (int, *Upstream) {
	for i, v := range us.Upstreams {
		if v.AppAlias == alias {
			return i, v
		}
	}
	return -1, nil
}

// note: must be called under protection of mutext lock
func (u *Upstream) getTarget(taskID string) (int, *Target) {
	for i, v := range u.Targets {
		if v.TaskID == taskID {
			return i, v
		}
	}
	return -1, nil
}

// Target
type Target struct {
	AppID      string  `json:"app_id"`
	AppAlias   string  `json:"app_alias"`
	VersionID  string  `json:"version_id"`
	AppVersion string  `json:"app_version"`
	TaskID     string  `json:"task_id"`
	TaskIP     string  `json:"task_ip"`
	TaskPort   uint32  `json:"task_port"`
	Weight     float64 `json:"weihgt"`
}

func (t *Target) url() (*url.URL, error) {
	s := fmt.Sprintf("http://%s:%d", t.TaskIP, t.TaskPort)
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid task url entry %s - [%v]", s, err)
	}

	return u, nil
}

func (t *Target) valid() error {
	if t == nil {
		return errors.New("nil targte")
	}
	if t.AppID == "" || t.TaskID == "" {
		return errors.New("app_id or task_id required")
	}
	if t.TaskIP == "" || t.TaskPort == 0 {
		return errors.New("task_ip or task_port required")
	}
	if !strings.HasSuffix(t.TaskID, "-"+t.AppID) {
		return errors.New("invalid task_id, must be suffixed by app_id")
	}
	return nil
}

// TargetChangeEvent
type TargetChangeEvent struct {
	Change string // add/del/update
	Target
}

func (ev TargetChangeEvent) String() string {
	return fmt.Sprintf("{%s: app:%s task:%s ip:%s:%d weight:%f}",
		ev.Change, ev.AppID, ev.TaskID, ev.TaskIP, ev.TaskPort, ev.Weight)
}