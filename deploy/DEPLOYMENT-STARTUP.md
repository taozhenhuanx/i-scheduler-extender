# i-scheduler-extender 部署文档（启动参数方案）

## 1. 方案说明

当前版本不读取 `NodeBalancer` CRD，也不运行 Operator。节点保护参数全部通过进程启动参数传入。程序只通过 `metrics.k8s.io/v1beta1` 读取节点实际 CPU 和内存使用率。

项目提供两种部署方式：

- `manifest.yaml`：在一个 Deployment 中运行自定义 `kube-scheduler` 和 Extender。业务 Pod 需要设置 `schedulerName: i-scheduler-extender`。
- `manifest-integrated.yaml`：只运行 Extender，复用已有 kube-scheduler。需要把 Extender 配置加入已有 scheduler，业务 Pod 使用默认 scheduler。

自建集群可以根据控制面权限选择方式。ACK 托管集群通常不能修改控制面 kube-scheduler，建议使用第一种方式。

## 2. 前置条件

- 集群 API Server 可访问；
- Metrics Server 正常运行，`kubectl top nodes` 可以返回数据；
- Extender 镜像已推送到集群可访问的镜像仓库；
- kube-scheduler 镜像版本与目标 Kubernetes 集群兼容。

检查 Metrics：

```bash
kubectl top nodes
```

## 3. 构建镜像

```bash
cd /data/scheduler/i-scheduler-extender
make build-image IMG=registry.example.com/platform/i-scheduler-extender:v1
```

然后将清单中的镜像替换为实际地址：

```yaml
image: registry.example.com/platform/i-scheduler-extender:v1
```

## 4. 启动参数

两个部署清单都使用启动参数配置节点保护：

```yaml
args:
  - --check-interval=5
  - --cpu-weight=0.1
  - --mem-weight=0.9
  - --high-threshold=70
  - --recovery-threshold=65
  - --min-schedulable-nodes=4
  - --top-nodes-count=9
```

参数说明：

- `--check-interval`：检查周期，单位为秒；
- `--cpu-weight`、`--mem-weight`：CPU 和内存使用率权重；
- `--high-threshold`：综合使用率达到该值后进入 cordon 候选；
- `--recovery-threshold`：综合使用率低于该值时才恢复自动 cordon 的节点；
- `--min-schedulable-nodes`：至少保留的可调度节点数量；
- `--top-nodes-count`：只处理综合使用率最高的前 N 个节点。

可选的节点范围参数：

```yaml
args:
  - --node-selector=scheduler.example.com/managed=true
  - --ignore-nodes=node-control-plane-1,node-control-plane-2
```

`--node-selector` 是节点白名单，只有匹配标签的节点参与评估；不配置时默认所有节点参与。`--ignore-nodes` 按节点名排除节点。

### Top N 语义

`--top-nodes-count=9` 是硬限制：程序按综合使用率从高到低排序，只对前 9 个节点做高负载 cordon 判断。第 10 个节点即使使用率达到 95%，也不会因为本轮 Top N 规则被新 cordon。

如果希望所有受管节点都有机会参与处理，应将它设置为不小于白名单节点数量。`--min-schedulable-nodes` 优先级更高，不能因为 Top N 导致可调度节点少于该值。

## 5. 方式一：独立 kube-scheduler

### 5.1 部署

`manifest.yaml` 会在一个 Pod 中启动自定义 kube-scheduler 和 Extender，Extender 地址通过 `localhost:8080` 访问：

```text
自定义 kube-scheduler -> localhost:8080 -> i-scheduler-extender
          |
          +-------------> Kubernetes API Server
```

部署命令：

```bash
kubectl apply -f deploy/manifest.yaml
kubectl -n kube-system rollout status deployment/i-scheduler-extender
kubectl -n kube-system get pod -l component=i-scheduler-extender -o wide
```

该清单的 scheduler profile 是：

```yaml
profiles:
  - schedulerName: i-scheduler-extender
```

所以业务 Pod 需要：

```yaml
spec:
  schedulerName: i-scheduler-extender
```

### 5.2 测试

当前 Extender 的 Filter 逻辑要求节点存在 `priority.lixueduan.com` 标签。测试前执行：

```bash
kubectl label node <node-name> priority.lixueduan.com=10
```

测试 Pod：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: scheduler-extender-test
spec:
  schedulerName: i-scheduler-extender
  containers:
    - name: test
      image: busybox:1.36
      command: ["sleep", "3600"]
```

检查：

```bash
kubectl get pod scheduler-extender-test -o wide
kubectl describe pod scheduler-extender-test
kubectl -n kube-system logs deployment/i-scheduler-extender -c i-scheduler-extender --tail=100
```

### 5.3 修改参数

修改 `deploy/manifest.yaml` 中 Extender 容器的 `args`，然后重新应用：

```bash
kubectl apply -f deploy/manifest.yaml
kubectl -n kube-system rollout status deployment/i-scheduler-extender
```

参数修改会触发 Deployment 滚动更新，新的 Pod 启动后生效。不需要修改控制面节点上的 `/etc/kubernetes/manifests/kube-scheduler.yaml`。

## 6. 方式二：集成已有 kube-scheduler

这种方式只部署 Extender，已有 kube-scheduler 通过 Service 调用它。部署 Extender：

```bash
kubectl apply -f deploy/manifest-integrated.yaml
kubectl -n kube-system rollout status deployment/i-scheduler-extender
kubectl -n kube-system get svc i-scheduler-extender
```

在已有 `KubeSchedulerConfiguration` 中合并下面的配置：

```yaml
extenders:
  - urlPrefix: "http://i-scheduler-extender.kube-system.svc.cluster.local:8080"
    enableHTTPS: false
    filterVerb: filter
    prioritizeVerb: prioritize
    bindVerb: bind
    nodeCacheCapable: false
    weight: 1
```

注意：

- `apiVersion` 必须使用目标 kube-scheduler 实际支持的版本；
- 保留原有 leader election、认证、TLS 和 profiles 配置；
- 已有 `profiles` 时只合并 `extenders`，不要重复添加 profile；
- kube-scheduler 必须能访问 Extender Service DNS；
- 集成方式下业务 Pod 不需要设置 `schedulerName`。

### 集成方式 RBAC

当前节点保护代码需要：

```text
core/nodes            get, list, patch
core/pods/binding     create
metrics.k8s.io/nodes  get, list
```

`manifest.yaml` 为了示例快速运行使用了 `cluster-admin`，生产环境应拆分 ServiceAccount 并收紧权限。`manifest-integrated.yaml` 中原有的 `pods/binding` Role 只覆盖 Bind，不足以支持节点负载保护；采用集成方式前，需要由集群管理员审核并补充 Node 和 Metrics API 的最小权限。

## 7. ACK 托管集群

ACK 托管控制面通常不能编辑：

```text
/etc/kubernetes/manifests/kube-scheduler.yaml
```

不能只部署 `manifest-integrated.yaml` 就让 ACK 内置 scheduler 自动调用 Extender。推荐运行独立的自定义 scheduler，并让目标业务 Pod 设置：

```yaml
spec:
  schedulerName: i-scheduler-extender
```

ACK 部署注意事项：

- kube-scheduler 镜像版本应与 ACK 集群版本兼容；
- 不要假设控制面节点上的 kubeconfig 可以挂载到业务 Pod；
- 使用 ServiceAccount 的 in-cluster 配置连接 API Server；
- 不要把管理员 kubeconfig 或长期凭据写入镜像；
- 如果不希望业务 YAML 写 `schedulerName`，可以另行实现 Mutating Admission Webhook 自动注入，但 Webhook 不能代替自定义 scheduler；
- 本项目当前不包含 Webhook 实现。

## 8. 节点保护行为

每个检查周期执行：

```text
读取 Metrics API
    -> CPU 10% + 内存 90% 计算综合使用率
    -> 按综合使用率降序排序
    -> 取 Top N
    -> 超过 high-threshold 的节点执行 cordon
    -> 至少保留 min-schedulable-nodes 个可调度节点
```

程序只修改：

```yaml
spec:
  unschedulable: true
```

不会驱逐已有 Pod。只有带有 `scheduler.example.com/auto-cordoned=true` 标记的节点，才可能被本程序自动 uncordon；人工 cordon 的节点不会被程序恢复。

## 9. 检查、升级和回滚

```bash
kubectl -n kube-system get deployment i-scheduler-extender -o yaml
kubectl -n kube-system logs deployment/i-scheduler-extender -c i-scheduler-extender --tail=200
kubectl top nodes
```

升级镜像：

```bash
kubectl -n kube-system set image deployment/i-scheduler-extender \
  i-scheduler-extender=registry.example.com/platform/i-scheduler-extender:v2
kubectl -n kube-system rollout status deployment/i-scheduler-extender
```

回滚：

```bash
kubectl -n kube-system rollout undo deployment/i-scheduler-extender
```

删除独立 scheduler 部署：

```bash
kubectl delete -f deploy/manifest.yaml
```

删除集成部署：

```bash
kubectl delete -f deploy/manifest-integrated.yaml
```

如果使用集成方式，还需要先从已有 kube-scheduler 配置中移除 `extenders`，再删除 Extender Service。

## 10. 上线前检查

- `kubectl top nodes` 正常；
- `top-nodes-count`、`min-schedulable-nodes` 与节点总数匹配；
- 已设置节点白名单，避免误处理控制平面或特殊用途节点；
- 已确认 `priority.lixueduan.com` 标签和 Filter 逻辑符合业务预期；
- 已确认 cordon 不会驱逐已有 Pod；
- 已将 `cluster-admin` 替换为审核后的最小 RBAC；
- 已验证 scheduler、Extender、Metrics API Server 之间的网络连通性；
- 已准备参数、镜像和 scheduler 配置的回滚方案。
