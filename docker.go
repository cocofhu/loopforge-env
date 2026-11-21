package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dockerAPI talks to the Engine API over the local socket. Exec sessions need
// the hijacked stream of /exec/{id}/start, which the docker CLI cannot hand over.
type dockerAPI struct {
	sock   string
	client *http.Client
}

const dockerAPIPrefix = "/v1.41"

func newDockerAPI() *dockerAPI {
	sock := "/var/run/docker.sock"
	if h, ok := strings.CutPrefix(env("DOCKER_HOST", ""), "unix://"); ok {
		sock = h
	}
	d := &dockerAPI{sock: sock}
	d.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", d.sock)
		},
	}}
	return d
}

func (d *dockerAPI) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+dockerAPIPrefix+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(res.Body, 64*1024)).Decode(&e)
		if e.Message == "" {
			e.Message = res.Status
		}
		return fmt.Errorf("docker: %s", e.Message)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

// Container is one container that belongs to an environment: a compose service
// or an execution sandbox the platform started for a task.
type Container struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // service | sandbox
	Service   string    `json:"service,omitempty"`
	Image     string    `json:"image"`
	State     string    `json:"state"`
	Status    string    `json:"status"`
	Workspace string    `json:"workspace,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// envContainers lists an environment's containers. Sandboxes are recognised by
// a bind mount under the environment's data directory.
func (d *dockerAPI) envContainers(ctx context.Context, root, sl string) ([]Container, error) {
	dataDir := readDotEnv(filepath.Join(root, "envs", sl, "src", ".env"))["LOOPFORGE_DATA_DIR"]
	var raw []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		State   string
		Status  string
		Created int64
		Labels  map[string]string
		Mounts  []struct{ Source, Destination string }
	}
	if err := d.do(ctx, "GET", "/containers/json?all=1", nil, &raw); err != nil {
		return nil, err
	}
	out := []Container{}
	for _, c := range raw {
		ct := Container{ID: c.ID, Image: c.Image, State: c.State, Status: c.Status, CreatedAt: time.Unix(c.Created, 0)}
		if len(c.Names) > 0 {
			ct.Name = strings.TrimPrefix(c.Names[0], "/")
		}
		// Sandboxes run the environment's own image and inherit its compose labels;
		// only containers compose created itself carry a container number.
		if c.Labels["com.docker.compose.project"] == "lfenv-"+sl && c.Labels["com.docker.compose.container-number"] != "" {
			ct.Kind, ct.Service = "service", c.Labels["com.docker.compose.service"]
		} else if dataDir != "" {
			for _, m := range c.Mounts {
				if rel, ok := strings.CutPrefix(m.Source, dataDir+"/"); ok {
					ct.Kind = "sandbox"
					if m.Destination == "/workspace" || ct.Workspace == "" {
						ct.Workspace = rel
					}
				}
			}
		}
		if ct.Kind != "" {
			out = append(out, ct)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "service"
		}
		if out[i].Kind == "service" {
			return out[i].Service > out[j].Service // platform before mysql
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

type execSession struct {
	d    *dockerAPI
	id   string
	conn net.Conn
	r    *bufio.Reader
}

// startExec runs cmd with a TTY in the container and returns its raw stream.
func (d *dockerAPI) startExec(ctx context.Context, container string, cmd []string, cols, rows int) (*execSession, error) {
	var created struct {
		ID string `json:"Id"`
	}
	spec := map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": true,
		"Cmd": cmd, "Env": []string{"TERM=xterm-256color", "LANG=C.UTF-8"},
	}
	if err := d.do(ctx, "POST", "/containers/"+url.PathEscape(container)+"/exec", spec, &created); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.sock)
	if err != nil {
		return nil, err
	}
	body := `{"Detach":false,"Tty":true}`
	fmt.Fprintf(conn, "POST %s/exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n%s",
		dockerAPIPrefix, created.ID, len(body), body)
	r := bufio.NewReader(conn)
	res, err := http.ReadResponse(r, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols && res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		conn.Close()
		return nil, fmt.Errorf("docker exec start: %s %s", res.Status, strings.TrimSpace(string(msg)))
	}
	s := &execSession{d: d, id: created.ID, conn: conn, r: r}
	if cols > 0 && rows > 0 {
		_ = s.resize(ctx, cols, rows)
	}
	return s, nil
}

func (s *execSession) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *execSession) Write(p []byte) (int, error) { return s.conn.Write(p) }

func (s *execSession) resize(ctx context.Context, cols, rows int) error {
	return s.d.do(ctx, "POST", fmt.Sprintf("/exec/%s/resize?h=%d&w=%d", s.id, rows, cols), nil, nil)
}

type execState struct {
	Running  bool
	ExitCode int
	Pid      int
}

func (s *execSession) inspect(ctx context.Context) (execState, error) {
	var st execState
	err := s.d.do(ctx, "GET", "/exec/"+s.id+"/json", nil, &st)
	return st, err
}

func (s *execSession) Close() error { return s.conn.Close() }
