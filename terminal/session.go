package terminal

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const scrollbackSize = 64 * 1024 // 64 KB

// observer is a read-only WebSocket client that receives session stdout.
type observer struct {
	ch   chan []byte
	done chan struct{}
}

// session holds a running exec for one user, independent of any WS connection.
type session struct {
	mu          sync.Mutex
	writeMu     sync.Mutex // serializes writes to wsConn
	stdinWriter io.WriteCloser
	scrollback  []byte
	wsConn      *websocket.Conn            // currently attached WS; nil when nobody is connected
	observers   map[*websocket.Conn]*observer // admin observers (read-only)
	sizeQueue   *sizeQueue
	done        chan struct{} // closed when the exec exits
}

// newSession creates a persistent exec session in the given pod running command.
func newSession(client *kubernetes.Clientset, restCfg *rest.Config, namespace, podName string, command []string) (*session, error) {
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
			Command:   command,
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
		observers:   make(map[*websocket.Conn]*observer),
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

	// Pump stdout into scrollback, forward to attached WS, and broadcast to observers.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutReader.Read(buf)
			if n > 0 {
				msg := make([]byte, n+1)
				msg[0] = msgData
				copy(msg[1:], buf[:n])

				s.mu.Lock()
				s.scrollback = append(s.scrollback, buf[:n]...)
				if len(s.scrollback) > scrollbackSize {
					s.scrollback = s.scrollback[len(s.scrollback)-scrollbackSize:]
				}
				// Snapshot observers while holding the lock.
				obs := make([]*observer, 0, len(s.observers))
				for _, o := range s.observers {
					obs = append(obs, o)
				}
				s.mu.Unlock()

				s.writeWS(websocket.BinaryMessage, msg)

				for _, o := range obs {
					select {
					case o.ch <- msg:
					default: // drop if the observer is slow
					}
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
		// Close all observer channels.
		for _, o := range s.observers {
			close(o.ch)
		}
		s.observers = make(map[*websocket.Conn]*observer)
		s.mu.Unlock()
		if conn != nil {
			s.writeMu.Lock()
			_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n\033[31mSession ended.\033[0m\r\n"))
			s.writeMu.Unlock()
		}
	}()

	return s, nil
}

// writeWS sends a message to the currently attached WebSocket connection.
// Safe to call concurrently with other writeWS calls.
func (s *session) writeWS(msgType int, msg []byte) {
	s.mu.Lock()
	conn := s.wsConn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	s.writeMu.Lock()
	_ = conn.WriteMessage(msgType, msg)
	s.writeMu.Unlock()
}

// addObserver registers conn as a read-only observer of this session.
// The current scrollback is immediately replayed to the new observer.
func (s *session) addObserver(conn *websocket.Conn) {
	o := &observer{
		ch:   make(chan []byte, 256),
		done: make(chan struct{}),
	}
	go func() {
		defer close(o.done)
		for msg := range o.ch {
			_ = conn.WriteMessage(websocket.BinaryMessage, msg)
		}
	}()

	s.mu.Lock()
	// Queue scrollback replay before adding to the broadcast list so ordering is preserved.
	if len(s.scrollback) > 0 {
		payload := make([]byte, len(s.scrollback)+1)
		payload[0] = msgData
		copy(payload[1:], s.scrollback)
		o.ch <- payload
	}
	s.observers[conn] = o
	s.mu.Unlock()
}

// removeObserver unregisters conn as an observer and waits for its write goroutine to exit.
func (s *session) removeObserver(conn *websocket.Conn) {
	s.mu.Lock()
	o, ok := s.observers[conn]
	if ok {
		delete(s.observers, conn)
		close(o.ch)
	}
	s.mu.Unlock()
	if ok {
		<-o.done
	}
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

// sessionManager holds one session per key.
type sessionManager struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionManager() *sessionManager {
	return &sessionManager{sessions: make(map[string]*session)}
}

// get returns the session for key if it exists (alive or not).
func (sm *sessionManager) get(key string) (*session, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[key]
	return s, ok
}

// purgeUser removes all sessions belonging to userSub (keys of the form "userSub:N").
func (sm *sessionManager) purgeUser(userSub string) {
	prefix := userSub + ":"
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for key := range sm.sessions {
		if strings.HasPrefix(key, prefix) {
			delete(sm.sessions, key)
		}
	}
}

// connectedUsers returns the set of userSubs that have at least one active WS connection.
func (sm *sessionManager) connectedUsers() map[string]bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	users := make(map[string]bool)
	for key, s := range sm.sessions {
		idx := strings.LastIndex(key, ":")
		if idx < 0 {
			continue
		}
		userSub := key[:idx]
		s.mu.Lock()
		connected := s.wsConn != nil
		s.mu.Unlock()
		if connected {
			users[userSub] = true
		}
	}
	return users
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
