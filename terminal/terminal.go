package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	k8s "run.pmh.codes/run/k8s"
)

// Message prefixes for the WebSocket framing protocol.
// \x00 = stdin/stdout data, \x01 = resize event JSON, \x02 = ping (server replies \x02+usage JSON)
const (
	msgData   = 0x00
	msgResize = 0x01
	msgPing   = 0x02
)

type resizeMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type Handler struct {
	client        *kubernetes.Clientset
	restCfg       *rest.Config
	podManager    *k8s.PodManager
	usage         *k8s.UsageHandler
	namespace     string
	upgrader      websocket.Upgrader
	sessions      *sessionManager
	adminSessions *sessionManager // root exec sessions keyed by pod name
}

func New(client *kubernetes.Clientset, restCfg *rest.Config, podManager *k8s.PodManager, usage *k8s.UsageHandler, namespace, baseURL string) *Handler {
	origin, _ := url.Parse(baseURL)
	return &Handler{
		client:        client,
		restCfg:       restCfg,
		podManager:    podManager,
		usage:         usage,
		namespace:     namespace,
		sessions:      newSessionManager(),
		adminSessions: newSessionManager(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				o := r.Header.Get("Origin")
				if o == "" {
					return true
				}
				u, err := url.Parse(o)
				if err != nil {
					return false
				}
				return u.Host == origin.Host
			},
		},
	}
}

// ConnectedUsers returns the set of userSubs with at least one active WebSocket connection.
func (h *Handler) ConnectedUsers() map[string]bool {
	return h.sessions.connectedUsers()
}

// Restart purges all sessions for the user and deletes the pod so the next
// connection starts fresh.
func (h *Handler) Restart(w http.ResponseWriter, r *http.Request, userSub string) {
	h.sessions.purgeUser(userSub)
	if err := h.podManager.DeletePod(r.Context(), userSub); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeHTTP upgrades to WebSocket, then attaches the connection to the user's
// persistent exec session (creating pod + session on first connect).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, userSub, username string) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	writeLine := func(msg string) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n"+msg+"\r\n"))
	}

	progressBar := func(pct int, msg string) {
		const width = 24
		filled := pct * width / 100
		bar := "\r\033[K\033[33m["
		for i := 0; i < width; i++ {
			if i < filled {
				bar += "█"
			} else {
				bar += "░"
			}
		}
		bar += fmt.Sprintf("] %3d%%\033[0m  %s", pct, msg)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(bar))
	}

	tabIdx, _ := strconv.Atoi(r.URL.Query().Get("tab"))
	if tabIdx < 0 || tabIdx > 5 {
		tabIdx = 0
	}
	sessionKey := fmt.Sprintf("%s:%d", userSub, tabIdx)

	sess, err := h.sessions.getOrCreate(sessionKey, func() (*session, error) {
		writeLine("\033[33mStarting your environment...\033[0m")
		progressBar(0, "Initializing...")

		pod, err := h.podManager.EnsurePod(r.Context(), userSub, username, func(pct int, msg string) {
			progressBar(pct, msg)
		})
		if err != nil {
			return nil, err
		}

		progressBar(100, "Connected!")
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n"))

		return newSession(h.client, h.restCfg, h.namespace, pod.Name, []string{"su", "-", username})
	})
	if err != nil {
		log.Printf("session error for user %s: %v", userSub, err)
		writeLine("\033[31mFailed to start session: " + err.Error() + "\033[0m")
		return
	}

	sess.attach(conn)
	defer sess.detach(conn)

	// Push usage to the client periodically so it doubles as a keepalive.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				u, err := h.usage.GetUsage(ctx, userSub)
				if err != nil {
					continue
				}
				payload, err := json.Marshal(u)
				if err != nil {
					continue
				}
				out := make([]byte, 1+len(payload))
				out[0] = msgPing
				copy(out[1:], payload)
				sess.writeWS(websocket.BinaryMessage, out)
			}
		}
	}()

	// Read loop: forward input and resize events to the session.
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case msgData:
			sess.writeStdin(msg[1:])
		case msgResize:
			var rm resizeMsg
			if err := json.Unmarshal(msg[1:], &rm); err == nil {
				sess.resize(rm.Cols, rm.Rows)
			}
		}
	}
}

// ServeWatch upgrades to WebSocket and attaches a read-only observer to the target
// user's session, streaming live stdout to the admin without forwarding any stdin.
func (h *Handler) ServeWatch(w http.ResponseWriter, r *http.Request, targetUserSub string) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("watch websocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	tabIdx, _ := strconv.Atoi(r.URL.Query().Get("tab"))
	if tabIdx < 0 || tabIdx > 5 {
		tabIdx = 0
	}
	sessionKey := fmt.Sprintf("%s:%d", targetUserSub, tabIdx)

	sess, ok := h.sessions.get(sessionKey)
	if !ok || !sess.isAlive() {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n\033[31mNo active session for this user on tab "+strconv.Itoa(tabIdx)+".\033[0m\r\n"))
		return
	}

	sess.addObserver(conn)
	defer sess.removeObserver(conn)

	// Block until the observer disconnects (read loop discards input).
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// ServeAdminExec upgrades to WebSocket and attaches to a persistent root exec session
// inside the specified pod. The session survives reconnects (keyed by pod name).
func (h *Handler) ServeAdminExec(w http.ResponseWriter, r *http.Request, podName string) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("admin exec websocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	writeLine := func(msg string) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n"+msg+"\r\n"))
	}

	sess, err := h.adminSessions.getOrCreate(podName, func() (*session, error) {
		writeLine("\033[33mOpening root session in " + podName + "...\033[0m")
		return newSession(h.client, h.restCfg, h.namespace, podName, []string{"/bin/bash"})
	})
	if err != nil {
		log.Printf("admin exec error for pod %s: %v", podName, err)
		writeLine("\033[31mFailed to open root session: " + err.Error() + "\033[0m")
		return
	}

	sess.attach(conn)
	defer sess.detach(conn)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case msgData:
			sess.writeStdin(msg[1:])
		case msgResize:
			var rm resizeMsg
			if err := json.Unmarshal(msg[1:], &rm); err == nil {
				sess.resize(rm.Cols, rm.Rows)
			}
		}
	}
}

// sizeQueue implements remotecommand.TerminalSizeQueue using a buffered channel.
type sizeQueue struct {
	ch chan remotecommand.TerminalSize
}

func newSizeQueue() *sizeQueue {
	return &sizeQueue{ch: make(chan remotecommand.TerminalSize, 8)}
}

func (s *sizeQueue) push(size remotecommand.TerminalSize) {
	select {
	case s.ch <- size:
	default:
	}
}

func (s *sizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-s.ch
	if !ok {
		return nil
	}
	return &size
}
