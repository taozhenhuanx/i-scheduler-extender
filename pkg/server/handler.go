package server

import (
	"encoding/json"
	"net/http"

	"github.com/lixd96/i-scheduler-extender/pkg/extender"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// Handler 将 kube-scheduler 的 HTTP 请求交给共享的 Extender，沿用请求取消信号。
type Handler struct {
	ex *extender.Extender
}

// NewHandler 复用 manager 创建的 Extender，使各 HTTP 接口共享权重和内存预留。
func NewHandler(ex *extender.Extender) *Handler {
	return &Handler{ex: ex}
}

// Routes 注册调度协议和健康接口；/priority 保留为 /prioritize 的兼容别名。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/filter", h.Filter)
	mux.HandleFunc("/filter_onlyone", h.FilterOnlyOne)
	mux.HandleFunc("/priority", h.Prioritize)
	mux.HandleFunc("/prioritize", h.Prioritize)
	mux.HandleFunc("/bind", h.Bind)
	mux.HandleFunc("/health", h.Health)
	return mux
}

// Health 返回可供 Eureka 使用的进程存活响应，leader 初始化就绪由 manager 的 /readyz 检查。
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "UP"}); err != nil {
		klog.Errorf("[Health] failed to encode result: %v", err)
	}
}

// Filter 解析候选节点和待调度 Pod，按 Extender 协议在 HTTP 200 的响应体中返回业务错误。
func (h *Handler) Filter(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var args extenderv1.ExtenderArgs
	var result *extenderv1.ExtenderFilterResult

	// 将 kube-scheduler 发送的 JSON 请求解析为 Extender 参数。
	err := json.NewDecoder(r.Body).Decode(&args)
	if err != nil {
		// 按 Scheduler Extender 协议把解析错误写入响应体。
		result = &extenderv1.ExtenderFilterResult{
			Error: err.Error(),
		}
	} else {
		result = h.ex.FilterWithContext(r.Context(), args)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		klog.Errorf("[Filter] failed to encode result: %v", err)
	}
}

// FilterOnlyOne 提供可选的单节点选择接口，先做内存准入，再选择最高分节点。
func (h *Handler) FilterOnlyOne(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var args extenderv1.ExtenderArgs
	var result *extenderv1.ExtenderFilterResult

	err := json.NewDecoder(r.Body).Decode(&args)
	if err != nil {
		result = &extenderv1.ExtenderFilterResult{
			Error: err.Error(),
		}
	} else {
		result = h.ex.FilterOnlyOneWithContext(r.Context(), args)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		klog.Errorf("[Filter] failed to encode result: %v", err)
	}
}

// Prioritize 返回候选节点的实测负载分数，由 kube-scheduler 与原生插件分数合并。
func (h *Handler) Prioritize(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var args extenderv1.ExtenderArgs
	var result *extenderv1.HostPriorityList

	err := json.NewDecoder(r.Body).Decode(&args)
	if err != nil {
		result = &extenderv1.HostPriorityList{}
	} else {
		result = h.ex.PrioritizeWithContext(r.Context(), args)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		klog.Errorf("[Prioritize] failed to encode result: %v", err)
	}
}

// Bind 解析绑定参数，委托 Extender 重验内存、登记预留并调用 API Server 绑定 Pod。
func (h *Handler) Bind(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var args extenderv1.ExtenderBindingArgs
	var result *extenderv1.ExtenderBindingResult

	err := json.NewDecoder(r.Body).Decode(&args)
	if err != nil {
		result = &extenderv1.ExtenderBindingResult{
			Error: err.Error(),
		}
	} else {
		result = h.ex.BindWithContext(r.Context(), args)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		klog.Errorf("[Bind] failed to encode result: %v", err)
	}
}
