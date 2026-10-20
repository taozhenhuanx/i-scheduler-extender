package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	schedulerv1alpha1 "github.com/lixd96/i-scheduler-extender/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	extenderInitializationTimeout = 30 * time.Second
	extenderShutdownTimeout       = 5 * time.Second
)

var (
	_ manager.Runnable               = (*extenderHTTPServer)(nil)
	_ manager.LeaderElectionRunnable = (*extenderHTTPServer)(nil)
)

// extenderHTTPServer 将 leader 身份、启动初始化和 HTTP 监听绑定在同一生命周期内。
// standby 不监听调度端口，也不进入 Service Endpoint，避免各实例维护独立的内存预留。
type extenderHTTPServer struct {
	address    string
	handler    http.Handler
	initialize func(context.Context) error

	mu         sync.RWMutex
	ready      bool
	runningCtx context.Context
}

func newExtenderHTTPServer(address string, handler http.Handler, initialize func(context.Context) error) *extenderHTTPServer {
	return &extenderHTTPServer{address: address, handler: handler, initialize: initialize}
}

// NeedLeaderElection 明确要求 manager 获得 lease 后才能启动调度接口。
func (*extenderHTTPServer) NeedLeaderElection() bool { return true }

// Ready 除了检查监听状态，还直接检查生命周期 context，以便取消时立即撤下 readiness。
func (s *extenderHTTPServer) Ready(_ *http.Request) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.ready || s.runningCtx == nil || s.runningCtx.Err() != nil {
		return errors.New("scheduler extender is not ready: waiting for leader, initialization, or HTTP listener")
	}
	return nil
}

func (s *extenderHTTPServer) setReady(ctx context.Context, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runningCtx = ctx
	s.ready = ready
}

// Start 先加载策略和恢复预留，然后等待 Serve 开始 Accept；任一步失败都不会 Ready。
func (s *extenderHTTPServer) Start(ctx context.Context) error {
	s.setReady(ctx, false)
	defer s.setReady(ctx, false)
	if err := ctx.Err(); err != nil {
		return nil
	}
	initializationCtx, cancelInitialization := context.WithTimeout(ctx, extenderInitializationTimeout)
	err := s.initialize(initializationCtx)
	if err == nil {
		err = initializationCtx.Err()
	}
	cancelInitialization()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("initialize scheduler extender: %w", err)
	}
	if ctx.Err() != nil {
		return nil
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.address)
	if err != nil {
		return fmt.Errorf("listen for scheduler extender: %w", err)
	}
	defer listener.Close()
	httpServer := &http.Server{
		Addr:              s.address,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	// 第一次进入 Accept 说明 Serve 已经开始接收连接，而不只是占用了端口。
	servingListener := &acceptReadyListener{Listener: listener, onAccept: func() { s.setReady(ctx, true) }}
	serverError := make(chan error, 1)
	go func() {
		err := httpServer.Serve(servingListener)
		s.setReady(ctx, false)
		serverError <- err
	}()

	select {
	case err := <-serverError:
		return extenderServeError(err)
	case <-ctx.Done():
		// 先撤下 Endpoint 再等待正在处理的请求结束；超时后强制关闭连接。
		s.setReady(ctx, false)
		return stopExtenderServer(httpServer, serverError)
	}
}

type acceptReadyListener struct {
	net.Listener
	onAccept func()
	once     sync.Once
}

func (l *acceptReadyListener) Accept() (net.Conn, error) {
	l.once.Do(l.onAccept)
	return l.Listener.Accept()
}

func stopExtenderServer(httpServer *http.Server, serverError <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), extenderShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Shutdown 超时不会自行终止已有连接；显式 Close 避免 goroutine 和旧请求残留。
		_ = httpServer.Close()
		<-serverError
		return fmt.Errorf("shut down scheduler extender: %w", err)
	}
	return extenderServeError(<-serverError)
}

func extenderServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type scoringWeightSetter interface {
	SetWeights(cpuWeight, memoryWeight float64) error
}

// initializePolicyWeights 通过 APIReader 读取当前策略，避免 cache 初次协调前用默认值评分。
// 无策略或策略正在删除时采用默认权重；读取或校验失败会阻止服务就绪。
func initializePolicyWeights(ctx context.Context, reader client.Reader, scoring scoringWeightSetter) error {
	policy := &schedulerv1alpha1.SchedulerPolicy{}
	err := reader.Get(ctx, types.NamespacedName{Name: "cluster"}, policy)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read initial SchedulerPolicy: %w", err)
	}
	spec := schedulerv1alpha1.SchedulerPolicySpec{}
	if err == nil && policy.DeletionTimestamp.IsZero() {
		spec = policy.Spec
	}
	cpuWeight, memoryWeight, err := spec.ResolveWeights()
	if err != nil {
		return fmt.Errorf("resolve initial SchedulerPolicy weights: %w", err)
	}
	if err := scoring.SetWeights(cpuWeight, memoryWeight); err != nil {
		return fmt.Errorf("apply initial SchedulerPolicy weights: %w", err)
	}
	return nil
}
