# SchedulerPolicy 策略配置

节点负载保护策略通过集群级自定义资源 `SchedulerPolicy/cluster` 管理。CRD 定义位于 [`../api/v1alpha1/schedulerpolicy_types.go`](../api/v1alpha1/schedulerpolicy_types.go)，示例对象位于 [`../config/samples/scheduler_v1alpha1_schedulerpolicy.yaml`](../config/samples/scheduler_v1alpha1_schedulerpolicy.yaml)。

首次部署时安装 CRD 并创建示例策略：

```bash
kubectl apply -f config/crd/bases/scheduler.example.com_schedulerpolicies.yaml
kubectl wait --for=condition=Established --timeout=60s crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
```

策略样例：

```yaml
apiVersion: scheduler.example.com/v1alpha1
kind: SchedulerPolicy
metadata:
  name: cluster
spec:
  checkIntervalSeconds: 5
  cpuWeight: 10
  memoryWeight: 90
  highThreshold: 70
  recoveryThreshold: 65
  minSchedulableNodes: 4
  topNodesCount: 9
  nodeSelector:
    scheduler.example.com/managed: "true"
  ignoreNodes:
    - node-a
    - node-b
```

- `checkIntervalSeconds`：检查周期，单位为秒。
- `cpuWeight`、`memoryWeight`：调度评分和节点保护共用的整数权重百分比，取值 0–100，两者之和必须为 100。缺省为 10/90，支持显式 0/100、100/0 和 1/99；运行时统一转换成 0–1 的比例。
- `highThreshold`：综合使用率达到该值后进入 cordon 候选，取值为 0–100。
- `recoveryThreshold`：综合使用率低于该值时恢复由本程序 cordon 的节点；省略时使用 `highThreshold - 5`。
- `minSchedulableNodes`：至少保留的可调度节点数量。
- `topNodesCount`：只对综合使用率最高的前 N 个节点判断高负载 cordon。
- `nodeSelector`：节点标签白名单，必须同时满足列出的标签；空 map 表示所有节点都参与评估。
- `ignoreNodes`：忽略节点名称列表。

使用 `kubectl edit schedulerpolicy cluster` 或应用修改后的 YAML 更新策略。controller 会自动协调新值；`kubectl get schedulerpolicy cluster` 查看 Ready condition。

`topNodesCount: 9` 是硬限制。程序按综合使用率从高到低排序，只对前 9 个节点判断高负载 cordon。第 10 个节点即使使用率达到 95%，也不会因本轮 Top N 规则被新 cordon。如果希望所有受管节点都有机会参与处理，应将 `topNodesCount` 设为不小于白名单节点数；`minSchedulableNodes` 是保留可调度节点的下限。

进程启动配置仍位于 [`../config/manager/scheduler.yaml`](../config/manager/scheduler.yaml)，但只包含 metrics、健康检查、Extender 监听地址和 leader election 设置。修改这些 manager 进程参数后需要按 `Recreate` 策略重启 Deployment。

调度评分复用 `cpuWeight` 和 `memoryWeight`，并将加权使用率映射到 0–10 分。Filter/Bind 以内存实测值为基线，加入尚未被采样覆盖的预约和新 Pod 预算；新 Pod 优先使用正数 request，没有正数 request 时使用 limit。超过 Allocatable 的 80% 时设置 `scheduler.example.com/memory-blocked=true` 并排除节点；当前值加预留降到 75% 以下且新 Pod 不会突破 80% 时解除标签。

绑定成功不会立即清除预留。应用容器及可重启 init 容器启动后，新的 NodeMetrics 完整采样窗口覆盖其启动时间才释放；启动等待不设固定清账超时，Pod 删除、终止或 UID 替换时清理旧预约。leader 初始化会重建过渡期预算。指标必须有有效 `timestamp`/`window` 且不超过 2 分钟，过旧或无效指标阻止准入。详细生命周期见 [README](../README.md)。
