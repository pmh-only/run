package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	metricsv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

type UsageResponse struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
}

type usageCache struct {
	resp      UsageResponse
	expiresAt time.Time
}

type UsageHandler struct {
	metrics   *metricsv1beta1.Clientset
	pods      *PodManager
	namespace string
	mu        sync.Mutex
	cache     map[string]usageCache
}

func NewUsageHandler(restCfg *rest.Config, pods *PodManager, namespace string) (*UsageHandler, error) {
	mc, err := metricsv1beta1.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	return &UsageHandler{metrics: mc, pods: pods, namespace: namespace, cache: make(map[string]usageCache)}, nil
}

// GetUsage returns cached usage (5s TTL) to avoid hammering the metrics server
// when multiple tabs ping simultaneously.
func (h *UsageHandler) GetUsage(ctx context.Context, userSub string) (*UsageResponse, error) {
	h.mu.Lock()
	if c, ok := h.cache[userSub]; ok && time.Now().Before(c.expiresAt) {
		h.mu.Unlock()
		resp := c.resp
		return &resp, nil
	}
	h.mu.Unlock()

	resp, err := h.fetchUsage(ctx, userSub)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	h.cache[userSub] = usageCache{resp: *resp, expiresAt: time.Now().Add(5 * time.Second)}
	h.mu.Unlock()

	return resp, nil
}

func (h *UsageHandler) fetchUsage(ctx context.Context, userSub string) (*UsageResponse, error) {
	name := PodName(userSub)

	podMetrics, err := h.metrics.MetricsV1beta1().PodMetricses(h.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	pod, err := h.pods.client.CoreV1().Pods(h.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var cpuUsage, memUsage resource.Quantity
	for _, c := range podMetrics.Containers {
		if c.Name == "shell" {
			cpuUsage = c.Usage["cpu"]
			memUsage = c.Usage["memory"]
		}
	}

	var cpuLimit, memLimit resource.Quantity
	for _, c := range pod.Spec.Containers {
		if c.Name == "shell" {
			cpuLimit = c.Resources.Limits["cpu"]
			memLimit = c.Resources.Limits["memory"]
		}
	}

	var cpuPct, memPct float64
	if cpuLimit.MilliValue() > 0 {
		cpuPct = float64(cpuUsage.MilliValue()) / float64(cpuLimit.MilliValue()) * 100.0
	}
	if memLimit.Value() > 0 {
		memPct = float64(memUsage.Value()) / float64(memLimit.Value()) * 100.0
	}

	return &UsageResponse{CPUPercent: cpuPct, MemoryPercent: memPct}, nil
}

// GetAllUsage returns current CPU/memory utilization for all run pods.
// Pods without metrics (e.g. pending) are omitted from the map.
// The metrics-server List API does not reliably support label selectors, so we
// fetch all pod metrics and filter client-side against the known run pods.
func (h *UsageHandler) GetAllUsage(ctx context.Context) (map[string]*UsageResponse, error) {
	// Build limits map from pod specs — only run pods (app=run label).
	pods, err := h.pods.client.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=run",
	})
	if err != nil {
		return nil, err
	}

	type podLimits struct{ cpu, mem resource.Quantity }
	limits := make(map[string]podLimits, len(pods.Items))
	for _, pod := range pods.Items {
		for _, c := range pod.Spec.Containers {
			if c.Name == "shell" {
				limits[pod.Name] = podLimits{
					cpu: c.Resources.Limits["cpu"],
					mem: c.Resources.Limits["memory"],
				}
			}
		}
	}

	// Fetch all metrics without a label selector (metrics-server has limited
	// label selector support) and skip pods not in the limits map.
	podMetricsList, err := h.metrics.MetricsV1beta1().PodMetricses(h.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	result := make(map[string]*UsageResponse, len(limits))
	for _, pm := range podMetricsList.Items {
		lim, ok := limits[pm.Name]
		if !ok {
			continue
		}
		var cpuUsage, memUsage resource.Quantity
		for _, c := range pm.Containers {
			if c.Name == "shell" {
				cpuUsage = c.Usage["cpu"]
				memUsage = c.Usage["memory"]
			}
		}
		var cpuPct, memPct float64
		if lim.cpu.MilliValue() > 0 {
			cpuPct = float64(cpuUsage.MilliValue()) / float64(lim.cpu.MilliValue()) * 100.0
		}
		if lim.mem.Value() > 0 {
			memPct = float64(memUsage.Value()) / float64(lim.mem.Value()) * 100.0
		}
		result[pm.Name] = &UsageResponse{CPUPercent: cpuPct, MemoryPercent: memPct}
	}
	return result, nil
}

func (h *UsageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, userSub string) {
	resp, err := h.GetUsage(r.Context(), userSub)
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
