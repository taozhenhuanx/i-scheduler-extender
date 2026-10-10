# 节点负载保护启动参数说明

本服务不读取 `NodeBalancer` CRD，也不依赖 Operator。所有节点保护参数都在进程启动时通过命令行参数传入。修改参数后，需要更新 Deployment 并滚动重启 Pod。

## 参数

- `--check-interval=5`：检查周期，单位为秒。
- `--cpu-weight=0.1`：CPU 使用率权重。
- `--mem-weight=0.9`：内存使用率权重。
- `--high-threshold=70`：综合使用率达到 70% 后进入 cordon 候选。
- `--recovery-threshold=65`：综合使用率低于该值时，程序才会恢复自己之前 cordon 的节点；不设置时默认为 `high-threshold - 5`。
- `--min-schedulable-nodes=4`：至少保留 4 个可调度节点。
- `--top-nodes-count=9`：只处理综合使用率最高的前 9 个节点。
- `--node-selector=key=value`：节点白名单；只有匹配标签的节点参与评估。多个标签用逗号分隔。
- `--ignore-nodes=node-a,node-b`：按节点名排除，不参与评估和 cordon。

## 当前 Top N 语义

`topNodesCount` 是硬限制，不是配置来源。比如：

```yaml
args:
  - --top-nodes-count=9
```

程序会先按综合使用率从高到低排序，只对前 9 个节点做高负载 cordon 判断。第 10 个节点即使使用率达到 95%，也不会因为本轮 Top N 规则被新 cordon。

如果希望所有受管节点都有机会参与处理，把 `--top-nodes-count` 设置为不小于白名单节点数量。`--min-schedulable-nodes` 仍然是硬保护条件。

已经被本程序自动 cordon 的节点，即使后续修改了 Top N，也只有使用率低于恢复阈值时才会自动 uncordon；人工 cordon 的节点不会被程序恢复。

## Deployment 配置示例

```yaml
spec:
  template:
    spec:
      containers:
        - name: i-scheduler-extender
          image: lixd96/i-scheduler-extender:v1
          args:
            - --check-interval=5
            - --cpu-weight=0.1
            - --mem-weight=0.9
            - --high-threshold=70
            - --recovery-threshold=65
            - --min-schedulable-nodes=4
            - --top-nodes-count=9
            - --node-selector=scheduler.example.com/managed=true
            - --ignore-nodes=node-control-plane-1
```

修改 Deployment 后执行：

```bash
kubectl -n kube-system rollout restart deployment/i-scheduler-extender
kubectl -n kube-system rollout status deployment/i-scheduler-extender
kubectl -n kube-system logs deployment/i-scheduler-extender --since=2m
```

## 行为边界

1. 程序只修改 `Node.spec.unschedulable`，不会驱逐已有 Pod。
2. 被 cordon 的节点不会再接收新的普通 Pod 调度。
3. 白名单为空时，默认所有节点都参与评估；设置 `--node-selector` 后只管理匹配标签的节点。
4. 节点指标来自 `metrics.k8s.io/v1beta1`，集群需要正常部署 Metrics Server。
5. 本程序不创建 CRD、不运行 Operator，也不会读取你已有的 `NodeBalancer` 资源。
