package projects

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const portAttempts = 3

// portLease is the machine-wide handoff set. The kernel will reissue a port
// the moment a probe socket closes, so a port stays leased until the child
// is confirmed listening or the attempt is abandoned.
var (
	portMu    sync.Mutex
	portLease = map[int]struct{}{}
)

var errProcessExited = errors.New("process exited")

func (s *Service) spawnPublished(app StoreApp) (string, int, error) {
	s.halt(app.ID, app.RuntimePID, app.ArtifactPath)
	avoid := map[int]struct{}{}
	if p := portFromURL(s.baseURL); p > 0 {
		avoid[p] = struct{}{}
	}
	var last error
	for attempt := 1; attempt <= portAttempts; attempt++ {
		port, release, err := leasePort(avoid)
		if err != nil {
			return "", 0, ErrValidation{"Couldn't find a free localhost port: " + err.Error()}
		}
		url, pid, retry, startErr := s.startOn(app, port)
		release()
		if startErr == nil {
			return url, pid, nil
		}
		last = startErr
		if !retry {
			return "", 0, startErr
		}
	}
	return "", 0, last
}

// startOn runs the built binary on a leased port. retry is true only when
// the process died before accepting connections, which is the signature of
// another program taking the port in the handoff gap.
func (s *Service) startOn(app StoreApp, port int) (string, int, bool, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(app.ArtifactPath)
	cmd.Dir = filepathDir(app.ArtifactPath)
	if app.ProjectID != nil {
		if p, err := s.repo.GetProject(context.Background(), *app.ProjectID); err == nil {
			if wd, err := resolveWorkdir(p); err == nil {
				cmd.Dir = wd
			}
		}
	}
	cmd.Env = overrideEnv(os.Environ(), map[string]string{
		"ADDR":              addr,
		"PORT":              strconv.Itoa(port),
		"OOZIE_IMPROVE_URL": s.baseURL + "/improve/" + app.BundleSlug,
		"OOZIE_BEACON_URL":  s.baseURL + "/api/beacon/" + app.BundleSlug,
	})
	logTail := &tailBuf{max: 4000}
	cmd.Stdout = logTail
	cmd.Stderr = logTail
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", 0, false, ErrValidation{"Couldn't start the app: " + err.Error()}
	}
	s.procs.Store(app.ID, cmd)
	go func() { _ = cmd.Wait() }()
	if err := waitListening(addr, cmd, 8*time.Second); err != nil {
		exit := ""
		if cmd.ProcessState != nil {
			exit = " exit=" + cmd.ProcessState.String()
		}
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		s.halt(app.ID, pid, app.ArtifactPath)
		msg := strings.TrimSpace(logTail.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", 0, errors.Is(err, errProcessExited), ErrValidation{"App was not reachable on " + addr + ": " + msg + exit}
	}
	return "http://" + addr, cmd.Process.Pid, false, nil
}

func (s *Service) halt(id int64, pid int, artifact string) {
	if v, ok := s.procs.LoadAndDelete(id); ok {
		if cmd, ok := v.(*exec.Cmd); ok && cmd.Process != nil {
			killPID(cmd.Process.Pid)
		}
	}
	if pid > 1 && processCommand(pid) != "" && (artifact == "" || strings.Contains(processCommand(pid), artifact)) {
		killPID(pid)
	}
}

func (s *Service) StopRuntimes() {
	s.procs.Range(func(k, v any) bool {
		id, _ := k.(int64)
		cmd, _ := v.(*exec.Cmd)
		pid := 0
		artifact := ""
		if cmd != nil && cmd.Process != nil {
			pid = cmd.Process.Pid
			artifact = cmd.Path
		}
		s.halt(id, pid, artifact)
		return true
	})
}

func waitListening(addr string, cmd *exec.Cmd, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			return errProcessExited
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			// A stolen port can accept before our process is reaped. Only
			// the process we started counts as a successful bind.
			if cmd.ProcessState != nil || !processAlive(cmd) {
				return errProcessExited
			}
			return nil
		}
		time.Sleep(40 * time.Millisecond)
	}
	if cmd.ProcessState != nil {
		return errProcessExited
	}
	return fmt.Errorf("timed out")
}

func processAlive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	err := cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

// leasePort asks the kernel for an unused localhost port and holds it in
// portLease until release. avoid lists ports that must not be issued, such
// as the factory's own address. The probe socket is closed before return;
// the lease, not the socket, is what stops a second start from taking it.
func leasePort(avoid map[int]struct{}) (int, func(), error) {
	portMu.Lock()
	defer portMu.Unlock()
	var last error
	for i := 0; i < 32; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			last = err
			continue
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if _, skip := avoid[port]; skip {
			ln.Close()
			continue
		}
		if _, held := portLease[port]; held {
			ln.Close()
			continue
		}
		portLease[port] = struct{}{}
		ln.Close()
		return port, func() {
			portMu.Lock()
			delete(portLease, port)
			portMu.Unlock()
		}, nil
	}
	if last == nil {
		last = fmt.Errorf("every candidate was reserved or forbidden")
	}
	return 0, nil, last
}

func portFromURL(raw string) int {
	host := strings.TrimPrefix(raw, "http://")
	host = strings.TrimPrefix(host, "https://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

func tcpUp(raw string) bool {
	host := strings.TrimPrefix(raw, "http://")
	host = strings.TrimPrefix(host, "https://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func killPID(pid int) {
	if pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(50 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func processCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func overrideEnv(base []string, set map[string]string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := set[key]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}

func filepathDir(path string) string {
	i := strings.LastIndex(path, string(os.PathSeparator))
	if i <= 0 {
		return "."
	}
	return path[:i]
}

// tailBuf keeps the last max bytes written, so a chatty server cannot grow
// without bound while we still have a failure message if it dies early.
type tailBuf struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
