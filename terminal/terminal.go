package terminal

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	k8s "run.pmh.codes/run/k8s"
)

// Message prefixes for the WebSocket framing protocol.
// \x00 = stdin/stdout data, \x01 = resize event JSON
const (
	msgData   = 0x00
	msgResize = 0x01
)

type resizeMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type Handler struct {
	client     *kubernetes.Clientset
	restCfg    *rest.Config
	podManager *k8s.PodManager
	namespace  string
	upgrader   websocket.Upgrader
	sessions   *sessionManager
}

func New(client *kubernetes.Clientset, restCfg *rest.Config, podManager *k8s.PodManager, namespace, baseURL string) *Handler {
	origin, _ := url.Parse(baseURL)
	return &Handler{
		client:     client,
		restCfg:    restCfg,
		podManager: podManager,
		namespace:  namespace,
		sessions:   newSessionManager(),
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

	sess, err := h.sessions.getOrCreate(userSub, func() (*session, error) {
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

		return newSession(h.client, h.restCfg, h.namespace, pod.Name, username)
	})
	if err != nil {
		log.Printf("session error for user %s: %v", userSub, err)
		writeLine("\033[31mFailed to start session: " + err.Error() + "\033[0m")
		return
	}

	sess.attach(conn)
	defer sess.detach(conn)

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
