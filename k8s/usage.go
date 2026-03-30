package k8s

import (
	"context"
	"encoding/json"
	"net/http"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	metricsv1beta1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

type UsageResponse struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
}

type UsageHandler struct {
	metrics   *metricsv1beta1.Clientset
	pods      *PodManager
	namespace string
}

func NewUsageHandler(restCfg *rest.Config, pods *PodManager, namespace string) (*UsageHandler, error) {
	mc, err := metricsv1beta1.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	return &UsageHandler{metrics: mc, pods: pods, namespace: namespace}, nil
}

func (h *UsageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, userSub string) {
	name := PodName(userSub)

	// Get actual usage from metrics-server
	podMetrics, err := h.metrics.MetricsV1beta1().PodMetricses(h.namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}

	// Get limits from pod spec
	pod, err := h.pods.client.CoreV1().Pods(h.namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
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

	resp := UsageResponse{
		CPUPercent:    cpuPct,
		MemoryPercent: memPct,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
