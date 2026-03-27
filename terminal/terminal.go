package terminal

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
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
	namespace  string
	shell      string
	upgrader   websocket.Upgrader
}

func New(client *kubernetes.Clientset, restCfg *rest.Config, namespace, shell, baseURL string) *Handler {
	origin, _ := url.Parse(baseURL)
	return &Handler{
		client:    client,
		restCfg:   restCfg,
		namespace: namespace,
		shell:     shell,
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

// ServeHTTP upgrades to WebSocket and connects to a running pod's exec endpoint.
// The pod must already be running - callers should call EnsurePod before this.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, pod *corev1.Pod) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Send "connecting" status message to the browser
	_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\nConnecting to your environment...\r\n"))

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	// Relay stdout/stderr from pod to browser
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutReader.Read(buf)
			if n > 0 {
				msg := make([]byte, n+1)
				msg[0] = msgData
				copy(msg[1:], buf[:n])
				if werr := conn.WriteMessage(websocket.BinaryMessage, msg); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}()

	// Relay browser input to pod stdin
	sizeQueue := newSizeQueue()
	go func() {
		defer stdinWriter.Close()
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
				if _, err := stdinWriter.Write(msg[1:]); err != nil {
					return
				}
			case msgResize:
				var rm resizeMsg
				if err := json.Unmarshal(msg[1:], &rm); err == nil {
					sizeQueue.push(remotecommand.TerminalSize{Width: rm.Cols, Height: rm.Rows})
				}
			}
		}
	}()

	req := h.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(h.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "shell",
			Command:   []string{h.shell},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(h.restCfg, "POST", req.URL())
	if err != nil {
		log.Printf("spdy executor error: %v", err)
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\nFailed to connect to pod: "+err.Error()+"\r\n"))
		return
	}

	err = exec.StreamWithContext(r.Context(), remotecommand.StreamOptions{
		Stdin:             stdinReader,
		Stdout:            stdoutWriter,
		Stderr:            stdoutWriter,
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	})
	if err != nil && err != io.EOF {
		log.Printf("exec stream error: %v", err)
	}
	stdoutWriter.Close()
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
