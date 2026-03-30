package terminal

import (
	"context"
	"io"
	"log"
	"sync"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const scrollbackSize = 64 * 1024 // 64 KB

// session holds a running exec for one user, independent of any WS connection.
type session struct {
	mu          sync.Mutex
	stdinWriter io.WriteCloser
	scrollback  []byte
	wsConn      *websocket.Conn // currently attached WS; nil when nobody is connected
	sizeQueue   *sizeQueue
	done        chan struct{} // closed when the exec exits
}

func newSession(client *kubernetes.Clientset, restCfg *rest.Config, namespace, podName, username string) (*session, error) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	sq := newSizeQueue()

	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "shell",
			Command:   []string{"su", "-", username},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		stdinWriter.Close()
		stdoutWriter.Close()
		return nil, err
	}

	s := &session{
		stdinWriter: stdinWriter,
		sizeQueue:   sq,
		done:        make(chan struct{}),
	}

	// Run exec in background — outlives any single WS connection.
	go func() {
		defer close(s.done)
		defer stdoutWriter.Close()
		if err := exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
			Stdin:             stdinReader,
			Stdout:            stdoutWriter,
			Stderr:            stdoutWriter,
			Tty:               true,
			TerminalSizeQueue: sq,
		}); err != nil && err != io.EOF {
			log.Printf("exec session for pod %s ended: %v", podName, err)
		}
	}()

	// Pump stdout into scrollback and forward to any attached WS.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutReader.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])

				s.mu.Lock()
				s.scrollback = append(s.scrollback, data...)
				if len(s.scrollback) > scrollbackSize {
					s.scrollback = s.scrollback[len(s.scrollback)-scrollbackSize:]
				}
				conn := s.wsConn
				s.mu.Unlock()

				if conn != nil {
					msg := make([]byte, n+1)
					msg[0] = msgData
					copy(msg[1:], data)
					_ = conn.WriteMessage(websocket.BinaryMessage, msg)
				}
			}
			if err != nil {
				break
			}
		}
		// Notify attached WS that the session has ended.
		s.mu.Lock()
		conn := s.wsConn
		s.wsConn = nil
		s.mu.Unlock()
		if conn != nil {
			_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n\033[31mSession ended.\033[0m\r\n"))
		}
	}()

	return s, nil
}

// attach sets conn as the current WS for this session.
// Any previously attached connection is kicked. Scrollback is replayed to the new conn.
func (s *session) attach(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.wsConn != nil {
		_ = s.wsConn.WriteMessage(websocket.TextMessage, []byte("\r\n\033[33mDisconnected: resumed on another client.\033[0m\r\n"))
		_ = s.wsConn.Close()
	}

	// Replay scrollback so the new client sees recent output.
	if len(s.scrollback) > 0 {
		msg := make([]byte, len(s.scrollback)+1)
		msg[0] = msgData
		copy(msg[1:], s.scrollback)
		_ = conn.WriteMessage(websocket.BinaryMessage, msg)
	}

	s.wsConn = conn
}

// detach removes conn as the current WS (no-op if a different conn is attached).
func (s *session) detach(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wsConn == conn {
		s.wsConn = nil
	}
}

func (s *session) writeStdin(data []byte) { _, _ = s.stdinWriter.Write(data) }

func (s *session) resize(cols, rows uint16) {
	s.sizeQueue.push(remotecommand.TerminalSize{Width: cols, Height: rows})
}

func (s *session) isAlive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// sessionManager holds one session per user.
type sessionManager struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionManager() *sessionManager {
	return &sessionManager{sessions: make(map[string]*session)}
}

// getOrCreate returns the existing alive session for key, or calls create() to make one.
// create() is called outside the lock so pod creation doesn't block other users.
func (sm *sessionManager) getOrCreate(key string, create func() (*session, error)) (*session, error) {
	sm.mu.Lock()
	if s, ok := sm.sessions[key]; ok && s.isAlive() {
		sm.mu.Unlock()
		return s, nil
	}
	sm.mu.Unlock()

	s, err := create()
	if err != nil {
		return nil, err
	}

	sm.mu.Lock()
	sm.sessions[key] = s
	sm.mu.Unlock()
	return s, nil
}
