package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

type JobKind string

const (
	KindBuild   JobKind = "build"
	KindDestroy JobKind = "destroy"
)

type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusSucceeded JobStatus = "succeeded"
	StatusFailed    JobStatus = "failed"
	StatusCanceled  JobStatus = "canceled"
)

type Job struct {
	ID         string     `json:"id"`
	Kind       JobKind    `json:"kind"`
	Branch     string     `json:"branch"`
	Slug       string     `json:"slug"`
	Status     JobStatus  `json:"status"`
	Commit     string     `json:"commit,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

func (j *Job) Active() bool { return j.Status == StatusQueued || j.Status == StatusRunning }

type QueueStats struct {
	Concurrency int `json:"concurrency"`
	Building    int `json:"building"`
	Queued      int `json:"queued"`
}

// Queue runs lfenv.sh jobs. At most one job per environment runs at a time, and
// builds are limited to Concurrency; destroys only wait for their environment.
type Queue struct {
	mu          sync.Mutex
	jobs        []*Job
	procs       map[string]*exec.Cmd
	canceled    map[string]bool
	concurrency int
	script      string
	root        string
	keep        int
}

var ErrNotFound = errors.New("not found")

func NewQueue(cfg Config) (*Queue, error) {
	q := &Queue{
		procs:       map[string]*exec.Cmd{},
		canceled:    map[string]bool{},
		concurrency: cfg.Concurrency,
		script:      cfg.Script,
		root:        cfg.Root,
		keep:        300,
	}
	if err := os.MkdirAll(q.logDir(), 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(q.statePath())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &q.jobs); err != nil {
			return nil, fmt.Errorf("read %s: %w", q.statePath(), err)
		}
	}
	now := time.Now()
	for _, j := range q.jobs {
		if j.Status == StatusRunning {
			j.Status, j.Error, j.FinishedAt = StatusFailed, "interrupted by a platform restart", &now
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.save()
	q.dispatch()
	return q, nil
}

func (q *Queue) statePath() string        { return filepath.Join(q.root, "jobs.json") }
func (q *Queue) logDir() string           { return filepath.Join(q.root, "logs") }
func (q *Queue) LogPath(id string) string { return filepath.Join(q.logDir(), id+".log") }

// Enqueue adds a job unless an equivalent one is already waiting. A build
// requested while the same environment is building waits behind it, so the
// rebuild picks up commits pushed in the meantime. A destroy cancels pending
// and running builds of the environment first.
func (q *Queue) Enqueue(kind JobKind, branch, slug string) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.Slug == slug && j.Status == StatusQueued && j.Kind == kind {
			return j
		}
	}
	if kind == KindDestroy {
		for _, j := range q.jobs {
			if j.Slug == slug && j.Kind == KindBuild && j.Active() {
				q.cancelLocked(j)
			}
		}
	}
	j := &Job{ID: newID(), Kind: kind, Branch: branch, Slug: slug, Status: StatusQueued, CreatedAt: time.Now()}
	q.jobs = append(q.jobs, j)
	q.save()
	q.dispatch()
	return j
}

func (q *Queue) Cancel(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.ID == id {
			if !j.Active() {
				return fmt.Errorf("job is already %s", j.Status)
			}
			q.cancelLocked(j)
			q.save()
			q.dispatch()
			return nil
		}
	}
	return ErrNotFound
}

// CancelSlug cancels every active job of an environment and reports whether one
// was still running (its process exits asynchronously).
func (q *Queue) CancelSlug(slug string) (running bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.Slug == slug && j.Active() {
			running = running || j.Status == StatusRunning
			q.cancelLocked(j)
		}
	}
	q.save()
	return running
}

func (q *Queue) cancelLocked(j *Job) {
	if j.Status == StatusQueued {
		now := time.Now()
		j.Status, j.FinishedAt = StatusCanceled, &now
		return
	}
	cmd := q.procs[j.ID]
	if cmd == nil || q.canceled[j.ID] {
		return
	}
	q.canceled[j.ID] = true
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	go func() {
		time.Sleep(15 * time.Second)
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.procs[j.ID] == cmd {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}

// Shutdown stops running jobs so their process groups do not outlive the server.
func (q *Queue) Shutdown(timeout time.Duration) {
	q.mu.Lock()
	for _, j := range q.jobs {
		if j.Status == StatusRunning {
			q.cancelLocked(j)
		}
	}
	q.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		n := len(q.procs)
		q.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (q *Queue) Get(id string) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.ID == id {
			return *j, true
		}
	}
	return Job{}, false
}

// Snapshot returns jobs newest first, limited to n (0 means all).
func (q *Queue) Snapshot(n int) ([]Job, QueueStats) {
	q.mu.Lock()
	defer q.mu.Unlock()
	stats := QueueStats{Concurrency: q.concurrency}
	out := make([]Job, 0, len(q.jobs))
	for i := len(q.jobs) - 1; i >= 0; i-- {
		j := q.jobs[i]
		switch {
		case j.Status == StatusQueued:
			stats.Queued++
		case j.Status == StatusRunning && j.Kind == KindBuild:
			stats.Building++
		}
		if n == 0 || len(out) < n || j.Active() {
			out = append(out, *j)
		}
	}
	return out, stats
}

// BranchOf reports the branch an active job holds for slug.
func (q *Queue) BranchOf(slug string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.Slug == slug && j.Active() {
			return j.Branch, true
		}
	}
	return "", false
}

func (q *Queue) dispatch() {
	building := 0
	busy := map[string]bool{}
	for _, j := range q.jobs {
		if j.Status == StatusRunning {
			busy[j.Slug] = true
			if j.Kind == KindBuild {
				building++
			}
		}
	}
	for _, j := range q.jobs {
		if j.Status != StatusQueued || busy[j.Slug] {
			continue
		}
		if j.Kind == KindBuild && building >= q.concurrency {
			continue
		}
		busy[j.Slug] = true
		if j.Kind == KindBuild {
			building++
		}
		q.start(j)
		q.save()
	}
}

func (q *Queue) start(j *Job) {
	now := time.Now()
	j.Status, j.StartedAt = StatusRunning, &now
	fail := func(err error) {
		j.Status, j.Error, j.FinishedAt = StatusFailed, err.Error(), &now
	}
	f, err := os.OpenFile(q.LogPath(j.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fail(err)
		return
	}
	verb := "up"
	if j.Kind == KindDestroy {
		verb = "down"
	}
	fmt.Fprintf(f, "[lfenv] %s  lfenv.sh %s %s\n", now.Format("2006-01-02 15:04:05"), verb, j.Branch)
	cmd := exec.Command("bash", q.script, verb, j.Branch)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.Env = append(os.Environ(), "LFENV_ROOT="+q.root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		fail(err)
		return
	}
	q.procs[j.ID] = cmd
	go q.wait(j, cmd, f)
}

func (q *Queue) wait(j *Job, cmd *exec.Cmd, f *os.File) {
	err := cmd.Wait()
	var commit string
	if j.Kind == KindBuild {
		if out, e := exec.Command("git", "-C", filepath.Join(q.root, "envs", j.Slug, "src"), "rev-parse", "HEAD").Output(); e == nil {
			commit = string(trimSpace(out))
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	j.FinishedAt, j.Commit = &now, commit
	switch {
	case q.canceled[j.ID]:
		j.Status = StatusCanceled
	case err != nil:
		j.Status, j.Error = StatusFailed, err.Error()
	default:
		j.Status = StatusSucceeded
	}
	fmt.Fprintf(f, "\n[lfenv] %s  %s in %s\n", now.Format("2006-01-02 15:04:05"), j.Status, now.Sub(*j.StartedAt).Round(time.Second))
	f.Close()
	delete(q.procs, j.ID)
	delete(q.canceled, j.ID)
	q.prune()
	q.save()
	q.dispatch()
}

func (q *Queue) prune() {
	var finished []*Job
	for _, j := range q.jobs {
		if !j.Active() {
			finished = append(finished, j)
		}
	}
	if len(finished) <= q.keep {
		return
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].CreatedAt.Before(finished[b].CreatedAt) })
	drop := map[string]bool{}
	for _, j := range finished[:len(finished)-q.keep] {
		drop[j.ID] = true
		_ = os.Remove(q.LogPath(j.ID))
	}
	kept := q.jobs[:0]
	for _, j := range q.jobs {
		if !drop[j.ID] {
			kept = append(kept, j)
		}
	}
	q.jobs = kept
}

func (q *Queue) save() {
	data, err := json.MarshalIndent(q.jobs, "", "  ")
	if err != nil {
		log.Printf("encode jobs: %v", err)
		return
	}
	tmp := q.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		err = os.Rename(tmp, q.statePath())
	}
	if err != nil {
		log.Printf("save jobs: %v", err)
	}
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
