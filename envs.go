package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Env struct {
	Branch    string    `json:"branch"`
	Slug      string    `json:"slug"`
	Port      int       `json:"port,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	URL       string    `json:"url,omitempty"`
}

// slug mirrors slug() in lfenv.sh; both must map a branch to the same directory.
// A commit environment is named after the commit's first 12 hex digits.
func slug(branch string) string {
	if sha, ok := strings.CutPrefix(branch, "commit:"); ok && len(sha) >= 12 {
		branch = "commit-" + sha[:12]
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(branch) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		out[k] = strings.Trim(v, `'"`)
	}
	return out
}

type container struct{ service, state, status string }

// composeContainers groups the containers of every Compose project by project name.
func composeContainers(ctx context.Context) map[string][]container {
	out := map[string][]container{}
	raw, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project",
		"--format", `{{.Label "com.docker.compose.project"}}	{{.Label "com.docker.compose.service"}}	{{.State}}	{{.Status}}`).Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		p := strings.Split(line, "\t")
		if len(p) == 4 {
			out[p[0]] = append(out[p[0]], container{p[1], p[2], p[3]})
		}
	}
	return out
}

func listEnvs(ctx context.Context, root string) []Env {
	dirs, _ := filepath.Glob(filepath.Join(root, "envs", "*", "src", ".env"))
	containers := composeContainers(ctx)
	envs := make([]Env, 0, len(dirs))
	for _, path := range dirs {
		src := filepath.Dir(path)
		vars := readDotEnv(path)
		e := Env{Branch: vars["LFENV_BRANCH"], Slug: filepath.Base(filepath.Dir(src)), State: "absent"}
		e.Port, _ = strconv.Atoi(vars["AGENTFLOW_HTTP_PORT"])
		if st, err := os.Stat(path); err == nil {
			e.CreatedAt = st.ModTime()
		}
		// The checkout moves to the new commit before its build finishes; "deployed" is written once it serves.
		if deployed, err := os.ReadFile(filepath.Join(filepath.Dir(src), "deployed")); err == nil {
			e.Commit = string(bytes.TrimSpace(deployed))
		} else if head, err := exec.CommandContext(ctx, "git", "-C", src, "rev-parse", "HEAD").Output(); err == nil {
			e.Commit = string(bytes.TrimSpace(head))
		}
		for _, c := range containers[vars["COMPOSE_PROJECT_NAME"]] {
			if c.service != "platform" {
				continue
			}
			e.Detail = c.status
			switch {
			case c.state != "running":
				e.State = "stopped"
			case strings.Contains(c.status, "(healthy)"):
				e.State = "running"
			case strings.Contains(c.status, "unhealthy"):
				e.State = "unhealthy"
			default:
				e.State = "starting"
			}
		}
		envs = append(envs, e)
	}
	sort.Slice(envs, func(a, b int) bool { return envs[a].CreatedAt.After(envs[b].CreatedAt) })
	return envs
}

// Branches lists remote branch and tag names, refreshed at most once a minute.
type Branches struct {
	repo    string
	mu      sync.Mutex
	names   []string
	tags    []string
	fetched time.Time
}

func (b *Branches) List(ctx context.Context) ([]string, error) {
	names, _, err := b.Refs(ctx)
	return names, err
}

func (b *Branches) Tags(ctx context.Context) ([]string, error) {
	_, tags, err := b.Refs(ctx)
	return tags, err
}

// Refs returns branches sorted by name and tags newest version first.
func (b *Branches) Refs(ctx context.Context) ([]string, []string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.fetched) < time.Minute {
		return b.names, b.tags, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "--tags", b.repo).Output()
	if err != nil {
		if b.names != nil {
			return b.names, b.tags, nil
		}
		return nil, nil, err
	}
	names, tags := []string{}, []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if _, ref, ok := strings.Cut(line, "\trefs/heads/"); ok {
			names = append(names, ref)
		} else if _, ref, ok := strings.Cut(line, "\trefs/tags/"); ok && !strings.HasSuffix(ref, "^{}") {
			tags = append(tags, ref)
		}
	}
	sort.Strings(names)
	sort.Slice(tags, func(i, j int) bool { return versionLess(tags[j], tags[i]) })
	b.names, b.tags, b.fetched = names, tags, time.Now()
	return names, tags, nil
}

// versionLess orders names like `sort -V`: runs of digits compare as numbers.
func versionLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digitPrefix(a), digitPrefix(b)
		if da != "" && db != "" {
			na, _ := strconv.ParseUint(da, 10, 64)
			nb, _ := strconv.ParseUint(db, 10, 64)
			if na != nb {
				return na < nb
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
