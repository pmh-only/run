package k8s

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

var nonAlnum = regexp.MustCompile(`[^a-z0-9-]`)

type PodManager struct {
	client    *kubernetes.Clientset
	namespace string
	image     string
	cpuLimit  string
	memLimit  string
}

func NewPodManager(client *kubernetes.Clientset, namespace, image, cpuLimit, memLimit string) *PodManager {
	return &PodManager{
		client:    client,
		namespace: namespace,
		image:     image,
		cpuLimit:  cpuLimit,
		memLimit:  memLimit,
	}
}

// PodName returns a stable, DNS-safe pod name for a user sub.
func PodName(sub string) string {
	// Use a short hash suffix to handle arbitrary sub formats safely
	h := sha256.Sum256([]byte(sub))
	suffix := fmt.Sprintf("%x", h[:4])
	// Also sanitize the sub for readability (first 20 chars)
	sanitized := nonAlnum.ReplaceAllString(strings.ToLower(sub), "-")
	if len(sanitized) > 20 {
		sanitized = sanitized[:20]
	}
	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		sanitized = "user"
	}
	return fmt.Sprintf("run-%s-%s", sanitized, suffix)
}

// EnsurePod returns a running pod for the user, creating one if needed.
func (m *PodManager) EnsurePod(ctx context.Context, userSub string) (*corev1.Pod, error) {
	name := PodName(userSub)

	pod, err := m.client.CoreV1().Pods(m.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("get pod: %w", err)
	}

	if err == nil {
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return pod, nil
		case corev1.PodSucceeded, corev1.PodFailed:
			log.Printf("pod %s is in terminal phase %s, recreating", name, pod.Status.Phase)
			if delErr := m.client.CoreV1().Pods(m.namespace).Delete(ctx, name, metav1.DeleteOptions{}); delErr != nil {
				return nil, fmt.Errorf("delete terminal pod: %w", delErr)
			}
			// Fall through to create
		default:
			// Pending or unknown - wait for it
			return m.waitForPod(ctx, name)
		}
	}

	newPod := m.buildPod(name, userSub)
	created, err := m.client.CoreV1().Pods(m.namespace).Create(ctx, newPod, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Race condition: another request created it, just wait
			return m.waitForPod(ctx, name)
		}
		return nil, fmt.Errorf("create pod: %w", err)
	}

	log.Printf("created pod %s for user sub %s", created.Name, userSub)
	return m.waitForPod(ctx, name)
}

func (m *PodManager) waitForPod(ctx context.Context, name string) (*corev1.Pod, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	watcher, err := m.client.CoreV1().Pods(m.namespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return nil, fmt.Errorf("watch pod: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, fmt.Errorf("timed out waiting for pod %s to be ready", name)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, fmt.Errorf("watch channel closed for pod %s", name)
			}
			if event.Type == watch.Deleted {
				return nil, fmt.Errorf("pod %s was deleted while waiting", name)
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return pod, nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return nil, fmt.Errorf("pod %s entered terminal phase %s", name, pod.Status.Phase)
			}
		}
	}
}

func (m *PodManager) buildPod(name, userSub string) *corev1.Pod {
	automount := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.namespace,
			Labels: map[string]string{
				"app":      "run",
				"run/user": fmt.Sprintf("%x", sha256.Sum256([]byte(userSub)))[:16],
			},
			Annotations: map[string]string{
				"run/user-sub": userSub,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{
				{
					Name:  "shell",
					Image: m.image,
					Command: []string{
						"/bin/sh", "-c",
						// Keep the container alive so exec can connect
						"trap '' TERM; while true; do sleep 3600; done",
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(m.cpuLimit),
							corev1.ResourceMemory: resource.MustParse(m.memLimit),
						},
					},
					Stdin: true,
					TTY:   true,
				},
			},
		},
	}
}
