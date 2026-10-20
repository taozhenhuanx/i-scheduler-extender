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
- `minSchedulableNodes`：新增 cordon 前保留的受管可用节点数量。只计入 Ready、未删除、未 cordon、指标新鲜有效，且没有内存保护标记及 `NoSchedule`/`NoExecute` taint 的节点。
- `topNodesCount`：只对受管可用节点中综合使用率最高的前 N 个节点判断高负载 cordon，故障或无效指标节点不占名额。
- `nodeSelector`：节点标签白名单，必须同时满足列出的标签；空 map 表示所有节点都参与评估。节点退出选择范围时清理本控制器的自动 cordon。
- `ignoreNodes`：忽略节点名称列表。节点加入列表时清理本控制器的自动 cordon。

使用 `kubectl edit schedulerpolicy cluster` 或应用修改后的 YAML 更新策略。controller 会自动协调新值；`kubectl get schedulerpolicy cluster` 查看 Ready condition。

### 可用节点与错误状态

可用节点须同时满足上述健康及调度条件，并具有有效 CPU/内存使用量和正数 Allocatable；NodeMetrics 的 `timestamp`/`window` 采用与 Extender 相同的时效规则：样本不超过 2 分钟，未来时钟偏差不超过 10 秒。缺失、过期或非法指标不计入可用节点，也不会用于 cordon/uncordon 决策。

`minSchedulableNodes` 用于阻止新增 cordon 进一步减少可用节点。已有节点故障、taint、内存保护或人工 cordon 导致数量低于下限时，控制器停止新增 cordon；它不会自动修复故障或解除人工封禁。带容忍的业务可能仍能使用 taint 节点，本计数采用普通业务的保守口径，不针对单个 Pod 判断资源、亲和性或容忍。

新增 cordon 前重新读取节点，更新可用数量及候选资格。不同节点的状态不能通过同一个 API 事务更新，外部同时改变健康状态仍可能使实际数量低于下限。cordon 写入超时或断连时，API 可能已经生效，控制器会停止本轮后续新增 cordon，下一轮重新读取状态后再判断。

退出管理范围的自动 cordon 清理先于 Metrics API 读取，指标服务故障不会阻止该清理。指标和节点操作失败会汇总到 `Ready=False`，返回错误触发重试；其他节点仍继续处理。下一轮完整成功后恢复 `Ready=True`。删除策略时清理失败同样标记 `Ready=False`，保留 finalizer 等待重试。

### 管理员接管节点

自动 cordon 会写入 `scheduler.example.com/auto-cordoned=true`。每次更新前重新读取节点并确认标记和 UID，避免覆盖并发人工封禁或同名重建节点。未带标记的人工 cordon 不会被恢复；已人工 uncordon 节点残留的标记会清理。

在一个已经自动 cordon 的节点上再次执行 `kubectl cordon`，API 状态无法表达管理员接管意图。要保留人工封禁，先移除自动标记再 cordon：

```bash
kubectl annotate node node-a scheduler.example.com/auto-cordoned-
kubectl cordon node-a
```

策略范围只控制 NodeBalancer 的 cordon 管理。Extender 的内存保护仍作用于 kube-scheduler 提供的候选节点，加入 `ignoreNodes` 不会绕过内存准入。

`topNodesCount: 9` 是硬限制。程序将受管可用节点按综合使用率从高到低排序，只对前 9 个节点判断高负载 cordon。该排序的第 10 个节点即使使用率达到 95%，也不会因本轮 Top N 规则被新 cordon。如果希望所有受管节点都有机会参与处理，应将 `topNodesCount` 设为不小于白名单节点数；`minSchedulableNodes` 是新增 cordon 的可用节点保护下限。

进程启动配置仍位于 [`../config/manager/scheduler.yaml`](../config/manager/scheduler.yaml)，但只包含 metrics、健康检查、Extender 监听地址和 leader election 设置。修改这些 manager 进程参数后需要按 `Recreate` 策略重启 Deployment。

调度评分复用 `cpuWeight` 和 `memoryWeight`，并将加权使用率映射到 0–10 分。Filter/Bind 以内存实测值为基线，加入尚未被采样覆盖的预约和新 Pod 预算；新 Pod 优先使用正数 request，没有正数 request 时使用 limit。超过 Allocatable 的 80% 时设置 `scheduler.example.com/memory-blocked=true` 并排除节点；当前值加预留降到 75% 以下且新 Pod 不会突破 80% 时解除标签。

绑定成功不会立即清除预留。应用容器及可重启 init 容器启动后，新的 NodeMetrics 完整采样窗口覆盖其启动时间才释放；启动等待不设固定清账超时，Pod 删除、终止或 UID 替换时清理旧预约。leader 初始化会重建过渡期预算。指标必须有有效 `timestamp`/`window` 且不超过 2 分钟，过旧或无效指标阻止准入。详细生命周期见 [README](../README.md)。
