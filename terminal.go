package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	wsText   = 0x1
	wsBinary = 0x2
	wsClose  = 0x8
	wsPing   = 0x9
	wsPong   = 0xA
)

// wsConn is a minimal server side of RFC 6455: enough for a terminal session.
type wsConn struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		writeErr(w, http.StatusBadRequest, "需要 WebSocket 连接")
		return nil, errors.New("not a websocket request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "缺少 Sec-WebSocket-Key")
		return nil, errors.New("missing key")
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法升级连接")
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{c: conn, r: brw.Reader}, nil
}

func (ws *wsConn) write(op byte, p []byte) error {
	ws.wmu.Lock()
	defer ws.wmu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_ = ws.c.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := ws.c.Write(hdr); err != nil {
		return err
	}
	_, err := ws.c.Write(p)
	return err
}

func (ws *wsConn) writeJSON(v any) error {
	b, _ := json.Marshal(v)
	return ws.write(wsText, b)
}

// read returns the next data message, answering pings and closes on the way.
func (ws *wsConn) read() (byte, []byte, error) {
	var op byte
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(ws.r, h[:]); err != nil {
			return 0, nil, err
		}
		fin, code, masked := h[0]&0x80 != 0, h[0]&0x0F, h[1]&0x80 != 0
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(ws.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(ws.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if !masked || n > 1<<20 || uint64(len(msg))+n > 1<<20 {
			return 0, nil, errors.New("websocket: bad frame")
		}
		var mask [4]byte
		if _, err := io.ReadFull(ws.r, mask[:]); err != nil {
			return 0, nil, err
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(ws.r, p); err != nil {
			return 0, nil, err
		}
		for i := range p {
			p[i] ^= mask[i%4]
		}
		switch code {
		case wsPing:
			_ = ws.write(wsPong, p)
			continue
		case wsPong:
			continue
		case wsClose:
			_ = ws.write(wsClose, p)
			return 0, nil, io.EOF
		case 0: // continuation
		default:
			op = code
		}
		msg = append(msg, p...)
		if fin {
			return op, msg, nil
		}
	}
}

func (ws *wsConn) Close() error { return ws.c.Close() }

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	sl := r.PathValue("slug")
	if _, ok := s.branchOf(sl); !ok {
		writeErr(w, http.StatusNotFound, "环境不存在")
		return
	}
	list, err := s.docker.envContainers(r.Context(), s.cfg.Root, sl)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, list)
}

// sameOrigin rejects cross-site WebSocket handshakes, which browsers send
// with the session cookie attached.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

const shellCmd = `if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi`

// handleTerminal attaches a WebSocket to an interactive shell in one of the
// environment's containers. Binary frames carry terminal bytes both ways; text
// frames carry control messages: {"type":"resize","cols":..,"rows":..} from the
// browser, {"type":"error"|"exit",...} from the server.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	sl, id := r.PathValue("slug"), r.PathValue("id")
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "origin not allowed")
		return
	}
	if _, ok := s.branchOf(sl); !ok {
		writeErr(w, http.StatusNotFound, "环境不存在")
		return
	}
	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	fail := func(msg string) { _ = ws.writeJSON(map[string]string{"type": "error", "message": msg}) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	list, err := s.docker.envContainers(ctx, s.cfg.Root, sl)
	if err != nil {
		fail(err.Error())
		return
	}
	var target *Container
	for i := range list {
		if list[i].ID == id {
			target = &list[i]
		}
	}
	if target == nil {
		fail("容器不属于这个环境")
		return
	}
	if target.State != "running" {
		fail("容器未运行（" + target.Status + "）")
		return
	}
	cmd := shellCmd
	if target.Kind == "sandbox" {
		// runc rejects an exec WorkingDir on the sandbox's bind mount ("current working
		// directory is outside of container mount namespace root"); cd inside instead.
		cmd = "cd /workspace 2>/dev/null; " + shellCmd
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	ex, err := s.docker.startExec(ctx, target.ID, []string{"sh", "-c", cmd}, cols, rows)
	if err != nil {
		fail(err.Error())
		return
	}
	defer ex.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := ex.Read(buf)
			if n > 0 && ws.write(wsBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = ws.write(wsPing, nil)
			}
		}
	}()
	go func() {
		for {
			op, msg, err := ws.read()
			if err != nil {
				ex.Close()
				return
			}
			if op == wsBinary {
				if _, err := ex.Write(msg); err != nil {
					return
				}
				continue
			}
			var ctl struct {
				Type       string
				Cols, Rows int
			}
			if json.Unmarshal(msg, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
				_ = ex.resize(ctx, ctl.Cols, ctl.Rows)
			}
		}
	}()
	<-done

	st, err := ex.inspect(ctx)
	if err == nil && st.Running && st.Pid > 0 {
		// The browser went away: docker leaves a detached exec running, so hang it up.
		_ = syscall.Kill(st.Pid, syscall.SIGHUP)
		time.AfterFunc(3*time.Second, func() {
			if st, err := ex.inspect(context.Background()); err == nil && st.Running && st.Pid > 0 {
				_ = syscall.Kill(st.Pid, syscall.SIGKILL)
			}
		})
		return
	}
	if err == nil {
		_ = ws.writeJSON(map[string]any{"type": "exit", "code": st.ExitCode})
	}
	_ = ws.write(wsClose, []byte{0x03, 0xE8})
}
