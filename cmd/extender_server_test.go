package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	schedulerv1alpha1 "github.com/lixd96/i-scheduler-extender/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExtenderServerReadinessRequiresInitializationAndListener(t *testing.T) {
	address := unusedLocalAddress(t)
	initializing := make(chan struct{})
	initialized := make(chan struct{})
	s := newExtenderHTTPServer(address, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ready")
	}), func(ctx context.Context) error {
		close(initializing)
		select {
		case <-initialized:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if !s.NeedLeaderElection() {
		t.Fatal("extender must run only after acquiring leadership")
	}
	if err := s.Ready(nil); err == nil {
		t.Fatal("standby must not report readiness before Start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	<-initializing
	if err := s.Ready(nil); err == nil {
		t.Fatal("initialization in progress must not report readiness")
	}
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("listener must not open before initialization succeeds")
	}
	close(initialized)
	waitForServerReady(t, s)
	httpClient := &http.Client{Timeout: time.Second}
	response, err := httpClient.Get("http://" + address)
	if err != nil {
		t.Fatalf("ready server must accept real HTTP requests: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "ready" {
		t.Fatalf("unexpected HTTP response: %q, error %v", body, err)
	}
	cancel()
	if err := s.Ready(nil); err == nil {
		t.Fatal("cancellation must revoke readiness immediately")
	}
	waitForServerStopped(t, done)
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("listener must close when manager stops")
	}
}

func TestExtenderServerGracefullyWaitsForActiveRequest(t *testing.T) {
	address := unusedLocalAddress(t)
	requestEntered := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	defer release()
	s := newExtenderHTTPServer(address, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestEntered)
		<-releaseRequest
		_, _ = io.WriteString(w, "completed")
	}), func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	waitForServerReady(t, s)
	requestDone := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + address)
		if err != nil {
			requestDone <- err
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err == nil && string(body) != "completed" {
			err = fmt.Errorf("unexpected body: %s", body)
		}
		requestDone <- err
	}()
	select {
	case <-requestEntered:
	case err := <-requestDone:
		t.Fatalf("request failed before entering handler: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("request never entered handler")
	}
	cancel()
	if err := s.Ready(nil); err == nil {
		t.Fatal("draining server must not remain ready")
	}
	select {
	case err := <-done:
		t.Fatalf("server exited while request was active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if err := <-requestDone; err != nil {
		t.Fatalf("active request was not completed gracefully: %v", err)
	}
	waitForServerStopped(t, done)
}

func TestExtenderServerInitializationAndListenFailuresNeverReady(t *testing.T) {
	initializationErr := errors.New("cannot load initial policy")
	s := newExtenderHTTPServer(unusedLocalAddress(t), http.NotFoundHandler(), func(context.Context) error {
		return initializationErr
	})
	if err := s.Start(context.Background()); !errors.Is(err, initializationErr) {
		t.Fatalf("initialization failure must be returned: %v", err)
	}
	if err := s.Ready(nil); err == nil {
		t.Fatal("failed initialization must not report readiness")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s = newExtenderHTTPServer(listener.Addr().String(), http.NotFoundHandler(), func(context.Context) error { return nil })
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("occupied listener address must prevent startup")
	}
	if err := s.Ready(nil); err == nil {
		t.Fatal("failed listener must not report readiness")
	}
}

func TestExtenderServerCancellationStopsInitialization(t *testing.T) {
	address := unusedLocalAddress(t)
	initializing := make(chan struct{})
	s := newExtenderHTTPServer(address, http.NotFoundHandler(), func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("initialization must have a deadline")
		}
		close(initializing)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	<-initializing
	cancel()
	waitForServerStopped(t, done)
	if err := s.Ready(nil); err == nil {
		t.Fatal("cancelled initialization must not report readiness")
	}
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("cancelled initialization must not open a listener")
	}
}

func TestInitializePolicyWeights(t *testing.T) {
	deleting := metav1.Now()
	tests := []struct {
		name      string
		policy    *schedulerv1alpha1.SchedulerPolicy
		readErr   error
		setterErr error
		wantCPU   float64
		wantMem   float64
		wantError bool
	}{
		{name: "no policy uses defaults", readErr: apierrors.NewNotFound(schema.GroupResource{Group: "scheduler.example.com", Resource: "schedulerpolicies"}, "cluster"), wantCPU: 0.1, wantMem: 0.9},
		{name: "omitted weights use defaults", policy: &schedulerv1alpha1.SchedulerPolicy{}, wantCPU: 0.1, wantMem: 0.9},
		{name: "zero CPU survives initialization", policy: policyWithWeights(0, 100), wantCPU: 0, wantMem: 1},
		{name: "one percent CPU is a ratio", policy: policyWithWeights(1, 99), wantCPU: 0.01, wantMem: 0.99},
		{name: "invalid existing policy blocks startup", policy: policyWithWeights(10, 100), wantError: true},
		{name: "API failure blocks startup", readErr: errors.New("API unavailable"), wantError: true},
		{name: "weight apply failure blocks startup", policy: policyWithWeights(10, 90), setterErr: errors.New("apply failed"), wantError: true},
		{name: "deleting policy resets defaults", policy: &schedulerv1alpha1.SchedulerPolicy{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deleting}, Spec: policyWithWeights(0, 100).Spec}, wantCPU: 0.1, wantMem: 0.9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &initialPolicyReader{policy: tt.policy, err: tt.readErr}
			setter := &recordingWeightSetter{err: tt.setterErr}
			err := initializePolicyWeights(context.Background(), reader, setter)
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected initialization error: %v", err)
			}
			if reader.key != (types.NamespacedName{Name: "cluster"}) {
				t.Fatalf("must read singleton cluster policy: %+v", reader.key)
			}
			if !tt.wantError && (setter.cpu != tt.wantCPU || setter.memory != tt.wantMem || !setter.called) {
				t.Fatalf("got weights %.2f/%.2f, want %.2f/%.2f", setter.cpu, setter.memory, tt.wantCPU, tt.wantMem)
			}
		})
	}
}

func policyWithWeights(cpu, memory int32) *schedulerv1alpha1.SchedulerPolicy {
	return &schedulerv1alpha1.SchedulerPolicy{Spec: schedulerv1alpha1.SchedulerPolicySpec{CPUWeight: &cpu, MemoryWeight: &memory}}
}

type initialPolicyReader struct {
	policy *schedulerv1alpha1.SchedulerPolicy
	err    error
	key    types.NamespacedName
}

func (r *initialPolicyReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	r.key = key
	if r.err != nil {
		return r.err
	}
	r.policy.DeepCopyInto(obj.(*schedulerv1alpha1.SchedulerPolicy))
	return nil
}

func (*initialPolicyReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("unexpected List call")
}

type recordingWeightSetter struct {
	cpu, memory float64
	called      bool
	err         error
}

func (s *recordingWeightSetter) SetWeights(cpu, memory float64) error {
	s.cpu, s.memory, s.called = cpu, memory, true
	return s.err
}

func unusedLocalAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitForServerReady(t *testing.T, s *extenderHTTPServer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Ready(nil) == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server never became ready")
}

func waitForServerStopped(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected server shutdown error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}
