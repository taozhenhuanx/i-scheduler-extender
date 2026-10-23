# 项目学习索引

本轮目标：理解 `feat/kubebuilder-operator` 分支的整体结构，以及一个 Pod 如何通过 Extender 完成绑定。

分析基于本地提交 `b503972` 的源码，日期为 2026-10-23。本文没有重新查询集群；部署记录中的镜像、Pod IP 和健康状态属于历史记录。本轮阅读了已有测试用例，但没有执行测试。

## 阅读顺序

1. [整体架构](整体架构.md)：组件边界、模块分工、技术栈和配置入口。
2. [Pod 调度链路](modules/Pod调度链路.md)：Filter、Prioritize、Bind，以及内存预算何时释放。
3. [策略控制与设计取舍](modules/策略控制与设计取舍.md)：CRD、Reconcile、节点保护和后续优化方向。

部署操作沿用 [部署指南](../../deploy/部署指南.md)，参数说明沿用 [节点保护策略配置](../../deploy/节点保护策略配置.md)，功能验证沿用 [调度逻辑与验证](../../deploy/调度逻辑与验证.md)。学习文档重点补充调用关系和源码依据。

## 项目一句话

这是一个使用 Kubebuilder 组织的 Kubernetes Operator：通过 `SchedulerPolicy` 管理节点负载策略，同时提供 HTTP Extender，让已有 kube-scheduler 在调度时加入实测负载评分和内存准入判断。

## 源码阅读地图

| 目录 | 负责什么 | 建议从哪里读 |
|---|---|---|
| `cmd/` | 创建客户端、manager、控制器和 HTTP 服务；启动与关闭 | [main.go](../../cmd/main.go#L54) |
| `api/v1alpha1/` | 定义集群级策略及状态 | [schedulerpolicy_types.go](../../api/v1alpha1/schedulerpolicy_types.go#L19) |
| `internal/controller/` | 读取策略、执行协调、更新状态和删除清理 | [scheduler_policy_controller.go](../../internal/controller/scheduler_policy_controller.go#L55) |
| `pkg/server/` | 解析 HTTP JSON，返回 Extender 协议响应 | [handler.go](../../pkg/server/handler.go#L23) |
| `pkg/extender/` | 内存过滤、实测评分、绑定、过渡预算 | [filter.go](../../pkg/extender/filter.go#L21) |
| `pkg/nodeusage/` | 统一资源单位、用量、Pod 预算和指标时效计算 | [usage.go](../../pkg/nodeusage/usage.go#L25) |
| `pkg/rebalancer/` | 自动 cordon/uncordon，并保留人工操作所有权 | [node_balancer.go](../../pkg/rebalancer/node_balancer.go#L96) |
| `pkg/settings/` | 加载进程启动 YAML | [settings.go](../../pkg/settings/settings.go#L32) |
| `config/` | Kustomize 管理 CRD、RBAC、Deployment、Service 等资源 | [default](../../config/default/kustomization.yaml) |
| `deploy/` | 部署说明、控制平面配置示例、验证 Pod | [部署指南](../../deploy/部署指南.md) |

## 证据怎么读

- **源码事实**：当前函数或配置直接体现的行为，文档给出文件和行号入口。
- **设计分析**：根据行为讨论优点、代价；不能据此断定历史设计动机。
- **待确认**：集群实时状态、性能容量、故障下的完整表现，需要额外验证。

最适合下一步深入的是 `pendingMemoryByNode`：它把 API 中的 Pod 状态、容器启动时间与节点采样时间串在一起，也是这个项目最复杂的状态处理。
