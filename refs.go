package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Besides branches and pull/<n>, an environment can build tag:<name> or
// commit:<full sha>. ':' cannot occur in a git ref name, so neither can clash
// with a branch.
var (
	shaInput = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	ghRefURL = regexp.MustCompile(`(?i)^https?://github\.com/([^/\s]+)/([^/\s]+)/(commit|tree|releases/tag)/([^?#\s]+?)/?$`)
)

func validBranch(name string) bool {
	return name != "" && !strings.HasPrefix(name, "-") && !strings.ContainsAny(name, " \t\n\\~^:?*[") && !strings.Contains(name, "..")
}

// resolveRef maps input that is not a pull request to the ref lfenv.sh builds.
// A name that is both a branch and a tag builds the branch; hex that names
// neither is taken as a commit.
func (s *Server) resolveRef(ctx context.Context, in string) (string, int, error) {
	kind, name := "", in
	if m := ghRefURL.FindStringSubmatch(in); m != nil {
		if !strings.EqualFold(m[1]+"/"+m[2], s.pulls.repo) {
			return "", http.StatusBadRequest, fmt.Errorf("只支持 %s 的链接", s.pulls.repo)
		}
		name, _ = url.PathUnescape(m[4])
		switch strings.ToLower(m[3]) {
		case "commit":
			kind = "commit"
		case "releases/tag":
			kind = "tag"
		}
	} else if v, ok := strings.CutPrefix(in, "tag:"); ok {
		kind, name = "tag", strings.TrimSpace(v)
	} else if v, ok := strings.CutPrefix(in, "refs/tags/"); ok {
		kind, name = "tag", v
	} else if v, ok := strings.CutPrefix(in, "commit:"); ok {
		kind, name = "commit", strings.TrimSpace(v)
	}

	heads, tags, refsErr := s.branches.Refs(ctx)
	switch kind {
	case "":
		switch {
		case refsErr == nil && contains(heads, name):
			return name, 0, nil
		case refsErr == nil && contains(tags, name):
			return "tag:" + name, 0, nil
		case shaInput.MatchString(name):
			kind = "commit"
		case !validBranch(name):
			return "", http.StatusBadRequest, fmt.Errorf("分支名不合法")
		case refsErr != nil:
			return name, 0, nil // cannot check now; lfenv.sh reports a missing branch
		default:
			return "", http.StatusNotFound, fmt.Errorf("远端不存在分支、标签或提交 %s", name)
		}
	case "tag":
		if !validBranch(name) {
			return "", http.StatusBadRequest, fmt.Errorf("标签名不合法")
		}
		if refsErr == nil && !contains(tags, name) {
			return "", http.StatusNotFound, fmt.Errorf("远端不存在标签 %s", name)
		}
		return "tag:" + name, 0, nil
	}
	if !shaInput.MatchString(name) {
		return "", http.StatusBadRequest, fmt.Errorf("提交号需要 7 到 40 位十六进制")
	}
	full, err := s.resolveCommit(ctx, strings.ToLower(name))
	if err != nil {
		return "", http.StatusNotFound, err
	}
	return "commit:" + full, 0, nil
}

// resolveCommit expands a commit id to its full hash: from the local mirror,
// which has every branch's history, else from GitHub, which also knows commits
// that only live in pull requests.
func (s *Server) resolveCommit(ctx context.Context, sha string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", filepath.Join(s.cfg.Root, "mirror.git"), "rev-parse", "--verify", "--quiet", sha+"^{commit}").Output()
	if full := strings.TrimSpace(string(out)); err == nil && len(full) == 40 {
		return full, nil
	}
	if s.pulls.repo != "" {
		var c struct{ SHA string }
		apiCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := s.pulls.api(apiCtx, "/commits/"+sha, &c); err == nil && len(c.SHA) == 40 {
			return c.SHA, nil
		}
	}
	if len(sha) == 40 {
		return sha, nil // lfenv.sh fetches it by hash, or reports it missing
	}
	return "", fmt.Errorf("找不到提交 %s", sha)
}

var subjects sync.Map // commit -> subject line; commits never change

// commitSubject is the first line of a deployed commit's message, if the
// mirror has it.
func (s *Server) commitSubject(ctx context.Context, commit string) string {
	if commit == "" {
		return ""
	}
	if v, ok := subjects.Load(commit); ok {
		return v.(string)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", filepath.Join(s.cfg.Root, "mirror.git"), "log", "-1", "--format=%s", commit).Output()
	if err != nil {
		return ""
	}
	subject := strings.TrimSpace(string(out))
	subjects.Store(commit, subject)
	return subject
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.branches.Tags(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "无法读取标签列表: "+err.Error())
		return
	}
	writeJSON(w, tags)
}
