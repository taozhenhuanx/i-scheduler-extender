package server

import (
	"encoding/json"
	"net/http"

	"github.com/lixd96/i-scheduler-extender/pkg/extender"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// Handler 处理器
type Handler struct {
	ex *extender.Extender
}

func NewHandler(ex *extender.Extender) *Handler {
	return &Handler{ex: ex}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/filter", h.Filter)
	mux.HandleFunc("/filter_onlyone", h.FilterOnlyOne)
	mux.HandleFunc("/priority", h.Prioritize)
	mux.HandleFunc("/prioritize", h.Prioritize)
	mux.HandleFunc("/bind", h.Bind)
	return mux
}

func (h *Handler) Filter(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var args extenderv1.ExtenderArgs
	var result *extenderv1.ExtenderFilterResult

	// 传进来的Body Decode给args
	err := json.NewDecoder(r.Body).Decode(&args)
	if err != nil {
		// 返回结果？
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
