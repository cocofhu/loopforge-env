package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type postFile struct {
	Key     string    `json:"key"`
	Path    string    `json:"path"`
	Content string    `json:"content"`
	Version string    `json:"version"`
	ModTime time.Time `json:"modTime,omitzero"`
}

var postFileMu sync.Mutex

// postFiles are the files editable from the UI: the post script and, when
// LFENV_POST_CONFIG is set, the config it reads.
func (s *Server) postFiles() map[string]string {
	files := map[string]string{}
	if s.cfg.PostScript != "" {
		files["script"] = s.cfg.PostScript
	}
	if s.cfg.PostConfig != "" {
		files["config"] = s.cfg.PostConfig
	}
	return files
}

func readPostFile(key, path string) (postFile, error) {
	f := postFile{Key: key, Path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if st, err := os.Stat(path); err == nil {
		f.ModTime = st.ModTime()
	}
	f.Content, f.Version = string(b), contentVersion(b)
	return f, nil
}

func contentVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (s *Server) handlePostInitGet(w http.ResponseWriter, r *http.Request) {
	out := []postFile{}
	for _, key := range []string{"script", "config"} {
		path, ok := s.postFiles()[key]
		if !ok {
			continue
		}
		f, err := readPostFile(key, path)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, f)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, out)
}

func (s *Server) handlePostInitPut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	path, ok := s.postFiles()[key]
	if !ok {
		writeErr(w, http.StatusNotFound, "未配置该文件")
		return
	}
	var body struct{ Content, Version string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	content := []byte(body.Content)
	if err := checkPostFile(key, path, content); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	postFileMu.Lock()
	defer postFileMu.Unlock()
	cur, err := readPostFile(key, path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cur.Version != body.Version {
		writeErr(w, http.StatusConflict, "文件已被其他人修改，请重新加载后再保存")
		return
	}
	mode := fs.FileMode(0o600)
	if key == "script" {
		mode = 0o755
	}
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
		if key == "script" {
			mode |= 0o100
		}
	}
	if err := writeFileAtomic(path, content, mode); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, _ := readPostFile(key, path)
	writeJSON(w, f)
}

// checkPostFile keeps a broken edit from failing every first start: the config
// must be valid JSON, the script needs a shebang and must parse.
func checkPostFile(key, path string, b []byte) error {
	if key == "config" {
		if strings.HasSuffix(path, ".json") && !json.Valid(b) {
			var v any
			err := json.Unmarshal(b, &v)
			return fmt.Errorf("JSON 格式错误: %v", err)
		}
		return nil
	}
	first, _, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(first, "#!") {
		return errors.New("脚本第一行需要 #! 解释器声明，例如 #!/usr/bin/env python3")
	}
	var cmd *exec.Cmd
	switch {
	case strings.Contains(first, "python"):
		cmd = exec.Command("python3", "-c", `import sys
try:
    compile(sys.stdin.read(), "script", "exec")
except SyntaxError as e:
    print(f"第 {e.lineno} 行: {e.msg}\n{(e.text or '').rstrip()}")
    sys.exit(1)`)
	case strings.Contains(first, "bash"):
		cmd = exec.Command("bash", "-n")
	case strings.HasSuffix(strings.TrimSpace(first), "/sh") || strings.Contains(first, " sh"):
		cmd = exec.Command("sh", "-n")
	default:
		return nil
	}
	cmd.Stdin = bytes.NewReader(b)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if lines := strings.Split(msg, "\n"); len(lines) > 6 {
			msg = strings.Join(lines[len(lines)-6:], "\n")
		}
		if msg == "" {
			msg = err.Error()
		}
		return errors.New("脚本语法错误:\n" + msg)
	}
	return nil
}

// writeFileAtomic replaces path in one rename, so a build that starts the
// script meanwhile sees either the old or the new file, never half of one.
func writeFileAtomic(path string, b []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
