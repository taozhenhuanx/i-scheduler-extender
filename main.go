package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/extender"
	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	"github.com/lixd96/i-scheduler-extender/pkg/rebalancer"
	"github.com/lixd96/i-scheduler-extender/pkg/server"
)

func main() {
	// 负载评分和节点保护参数来自启动参数；修改配置后重启该 Deployment 生效。
	checkInterval := flag.Int("check-interval", 5, "节点检查间隔，单位：秒")
	cpuWeight := flag.String("cpu-weight", "0.1", "CPU 权重，支持 0.1、10 或 10%")
	memWeight := flag.String("mem-weight", "0.9", "内存权重，支持 0.9、90 或 90%")
	highThreshold := flag.Float64("high-threshold", 70, "高负载阈值，单位：百分比")
	recoveryThreshold := flag.Float64("recovery-threshold", -1, "恢复阈值；小于 0 时自动使用 high-threshold-5")
	minSchedulableNodes := flag.Int("min-schedulable-nodes", 4, "至少保留的可调度节点数")
	topNodesCount := flag.Int("top-nodes-count", 9, "只处理综合使用率最高的前 N 个节点")
	nodeSelector := flag.String("node-selector", "", "节点白名单标签选择器，例如 workload-type=general,env=prod")
	ignoreNodes := flag.String("ignore-nodes", "", "忽略的节点名称，多个节点用逗号分隔")
	flag.Parse()

	if *checkInterval < 1 || *minSchedulableNodes < 1 || *topNodesCount < 1 {
		log.Fatal("check-interval、min-schedulable-nodes 和 top-nodes-count 必须大于 0")
	}
	selector, err := rebalancer.ParseNodeSelector(*nodeSelector)
	if err != nil {
		log.Fatalf("invalid node selector: %v", err)
	}
	ignoredNodes, err := rebalancer.ParseIgnoreNodes(*ignoreNodes)
	if err != nil {
		log.Fatalf("invalid ignored nodes: %v", err)
	}
	cpuScoreWeight, memoryScoreWeight, err := nodeusage.ParseWeights(*cpuWeight, *memWeight)
	if err != nil {
		log.Fatalf("invalid scoring weights: %v", err)
	}

	ex := extender.NewExtenderWithWeights(cpuScoreWeight, memoryScoreWeight)
	h := server.NewHandler(ex)
	config := rebalancer.Config{
		CheckInterval:       time.Duration(*checkInterval) * time.Second,
		CPUWeight:           *cpuWeight,
		MemoryWeight:        *memWeight,
		NodeSelector:        selector,
		IgnoreNodes:         ignoredNodes,
		HighThreshold:       *highThreshold,
		RecoveryThreshold:   *recoveryThreshold,
		MinSchedulableNodes: *minSchedulableNodes,
		TopNodesCount:       *topNodesCount,
	}
	balancer, err := rebalancer.New(ex.ClientSet, ex.DynamicClient, config)
	if err != nil {
		log.Fatalf("failed to create node balancer: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go balancer.Run(ctx)

	http.HandleFunc("/filter", h.Filter)
	http.HandleFunc("/filter_onlyone", h.FilterOnlyOne)
	http.HandleFunc("/priority", h.Prioritize)
	http.HandleFunc("/prioritize", h.Prioritize)
	http.HandleFunc("/bind", h.Bind)

	httpServer := &http.Server{Addr: ":8080"}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server stopped: %v", err)
	}
}
