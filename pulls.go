package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A pull request environment is keyed by the ref "pull/<n>" wherever other
// environments use a branch name; lfenv.sh fetches refs/pull/<n>/head for it.
var (
	prRef   = regexp.MustCompile(`^pull/([0-9]+)$`)
	prInput = regexp.MustCompile(`(?i)^(?:#|pr\s*#?\s*|pull/)([0-9]+)$`)
	prURL   = regexp.MustCompile(`(?i)^https?://github\.com/([^/\s]+)/([^/\s]+)/pull/([0-9]+)`)
	ghRepo  = regexp.MustCompile(`(?i)^(?:https?://|git@)github\.com[/:]([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)
)

// prNumber returns n for the ref pull/<n>, or 0 for a branch.
func prNumber(ref string) int {
	m := prRef.FindStringSubmatch(ref)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

type PR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"` // open | closed | merged
	Draft  bool   `json:"draft,omitempty"`
	Head   string `json:"head"` // owner:branch
	Author string `json:"author"`
	URL    string `json:"url"`
}

type prEntry struct {
	pr      *PR
	fetched time.Time
}

// Pulls reads pull requests of the configured GitHub repository. Details are
// cached because the UI polls; unauthenticated API calls allow 60 an hour.
type Pulls struct {
	repoURL string
	repo    string // owner/name, empty when the repository is not on GitHub
	token   string
	client  *http.Client

	mu       sync.Mutex
	info     map[int]prEntry
	inflight map[int]bool

	openMu      sync.Mutex // held across the API call, so it must not be mu
	open        []PR
	openFetched time.Time

	etagMu sync.Mutex
	etags  map[string]etagged
}

func newPulls(repoURL, token string) *Pulls {
	p := &Pulls{repoURL: repoURL, token: token, client: &http.Client{Timeout: 10 * time.Second},
		info: map[int]prEntry{}, inflight: map[int]bool{}, etags: map[string]etagged{}}
	if m := ghRepo.FindStringSubmatch(repoURL); m != nil {
		p.repo = m[1] + "/" + m[2]
	}
	return p
}

// Parse recognises pull request input: #478, PR 478, pull/478 or the PR's URL.
// ok is false for anything else, which is then treated as a branch name.
func (p *Pulls) Parse(in string) (n int, ok bool, err error) {
	if m := prURL.FindStringSubmatch(in); m != nil {
		if !strings.EqualFold(m[1]+"/"+m[2], p.repo) {
			return 0, true, fmt.Errorf("只支持 %s 的 PR", p.repo)
		}
		n, _ = strconv.Atoi(m[3])
	} else if m := prInput.FindStringSubmatch(in); m != nil {
		n, _ = strconv.Atoi(m[1])
	} else {
		return 0, false, nil
	}
	if p.repo == "" {
		return 0, true, errors.New("仓库不在 GitHub 上，不支持按 PR 构建")
	}
	if n <= 0 {
		return 0, true, errors.New("PR 编号不合法")
	}
	return n, true, nil
}

// Exists asks git, which has no rate limit, whether refs/pull/<n>/head exists.
func (p *Pulls) Exists(ctx context.Context, n int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "ls-remote", p.repoURL, fmt.Sprintf("refs/pull/%d/head", n)).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

type ghPull struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	State    string  `json:"state"`
	Draft    bool    `json:"draft"`
	MergedAt *string `json:"merged_at"`
	HTMLURL  string  `json:"html_url"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Label string `json:"label"`
	} `json:"head"`
}

func (g ghPull) pr() *PR {
	state := g.State
	if g.MergedAt != nil {
		state = "merged"
	}
	return &PR{Number: g.Number, Title: g.Title, State: state, Draft: g.Draft, Head: g.Head.Label, Author: g.User.Login, URL: g.HTMLURL}
}

type etagged struct {
	etag string
	body []byte
}

// api GETs a repository endpoint. Responses are revalidated with their ETag:
// a 304 does not count against GitHub's rate limit.
func (p *Pulls) api(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+p.repo+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "lfenv")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	p.etagMu.Lock()
	cached, ok := p.etags[path]
	p.etagMu.Unlock()
	if ok {
		req.Header.Set("If-None-Match", cached.etag)
	}
	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var body []byte
	switch {
	case res.StatusCode == http.StatusNotModified && ok:
		body = cached.body
	case res.StatusCode == http.StatusOK:
		if body, err = io.ReadAll(io.LimitReader(res.Body, 8<<20)); err != nil {
			return err
		}
		if tag := res.Header.Get("ETag"); tag != "" {
			p.etagMu.Lock()
			p.etags[path] = etagged{tag, body}
			p.etagMu.Unlock()
		}
	default:
		return fmt.Errorf("GitHub API %s", res.Status)
	}
	return json.Unmarshal(body, out)
}

// Cached returns what is known about PR n without waiting on GitHub; stale or
// missing entries are refreshed in the background.
func (p *Pulls) Cached(n int) *PR {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.info[n]
	if p.repo != "" && (!ok || time.Since(e.fetched) > 5*time.Minute) && !p.inflight[n] {
		p.inflight[n] = true
		go p.refresh(n)
	}
	return e.pr
}

func (p *Pulls) refresh(n int) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var g ghPull
	err := p.api(ctx, fmt.Sprintf("/pulls/%d", n), &g)
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inflight, n)
	e := p.info[n]
	if err == nil {
		e.pr = g.pr()
	}
	// A failure keeps the old details and waits a full period before retrying.
	e.fetched = time.Now()
	p.info[n] = e
}

// Open lists open pull requests for the branch picker, refreshed every 3 minutes.
func (p *Pulls) Open(ctx context.Context) ([]PR, error) {
	if p.repo == "" {
		return []PR{}, nil
	}
	p.openMu.Lock()
	defer p.openMu.Unlock()
	if p.open != nil && time.Since(p.openFetched) < 3*time.Minute {
		return p.open, nil
	}
	var list []ghPull
	err := p.api(ctx, "/pulls?state=open&per_page=100&sort=updated&direction=desc", &list)
	p.openFetched = time.Now()
	if err != nil {
		if p.open != nil {
			return p.open, nil
		}
		return nil, err
	}
	p.open = make([]PR, 0, len(list))
	p.mu.Lock()
	for _, g := range list {
		pr := g.pr()
		p.open = append(p.open, *pr)
		p.info[pr.Number] = prEntry{pr: pr, fetched: p.openFetched}
	}
	p.mu.Unlock()
	return p.open, nil
}
