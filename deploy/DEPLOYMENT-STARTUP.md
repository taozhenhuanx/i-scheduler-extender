# i-scheduler-extender Operator 部署文档

## 1. 方案说明

进程由 Kubebuilder/controller-runtime manager 管理。集群级 `SchedulerPolicy/cluster` 是调度策略的期望状态；controller 监听策略和 Node 变化，并按 CR 的 spec 协调节点负载保护和 Scheduler Extender 的打分权重。同一 manager 还托管 Scheduler Extender HTTP 接口。调度打分和节点保护通过 `metrics.k8s.io/v1beta1` 读取节点实际 CPU 和内存使用率。调度 Filter 会按已调度 Pod 的内存 request/limit 与新 Pod 的 request/limit 预估内存占用；超过节点 Allocatable 的 80% 时标记节点并拒绝新 Pod，降至 75% 后解除标记。

80% 是调度预测门槛，不是物理内存绝对上限：Metrics API 存在采样延迟，Pod 可能超过 request，且未经过此 Extender 的调度路径不受拦截。实际拒绝由 Extender 的 Filter/Bind 执行。

项目提供三种部署方式：

- `make deploy`：通过 Kubebuilder 的 Kustomize 配置安装 CRD、manager 和 Extender Service；首次部署还需要创建 `SchedulerPolicy/cluster`。
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

## 4. 配置文件

进程启动选项集中在 [`../config/manager/scheduler.yaml`](../config/manager/scheduler.yaml)：

```yaml
metricsBindAddress: "0"
healthProbeBindAddress: ":8082"
extenderBindAddress: ":8080"
leaderElection: true
```

该文件只配置 manager、metrics、健康检查和 Extender HTTP 服务，不包含集群调度策略。策略通过集群级 `SchedulerPolicy/cluster` CR 设置：

```bash
make install
kubectl wait --for=condition=Established --timeout=60s crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
```

`SchedulerPolicy` 字段包括检查周期、CPU/内存权重百分比、高负载/恢复阈值、可调度节点下限、Top N、节点标签选择器和忽略节点列表。示例与字段注释见 [`../config/samples/scheduler_v1alpha1_schedulerpolicy.yaml`](../config/samples/scheduler_v1alpha1_schedulerpolicy.yaml) 和 [`../api/v1alpha1/schedulerpolicy_types.go`](../api/v1alpha1/schedulerpolicy_types.go)。更新策略对象后 controller 会自动协调；`kubectl get schedulerpolicy cluster` 可查看 Ready 状态。

### Top N 语义

`spec.topNodesCount: 9` 是硬限制：程序按综合使用率从高到低排序，只对前 9 个节点做高负载 cordon 判断。第 10 个节点即使使用率达到 95%，也不会因为本轮 Top N 规则被新 cordon。

如果希望所有受管节点都有机会参与处理，应将它设置为不小于白名单节点数量。`spec.minSchedulableNodes` 优先级更高，不能因为 Top N 导致可调度节点少于该值。

## 5. 方式一：独立 kube-scheduler

### 5.1 部署

`manifest.yaml` 会在一个 Pod 中启动自定义 kube-scheduler 和 Extender，Extender 地址通过 `localhost:8080` 访问：

```text
自定义 kube-scheduler -> localhost:8080 -> i-scheduler-extender
          |
          +-------------> Kubernetes API Server
```

部署命令（CRD 必须先于使用它的 manager 安装）：

```bash
kubectl apply -f config/crd/bases/scheduler.example.com_schedulerpolicies.yaml
kubectl wait --for=condition=Established --timeout=60s crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
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

Extender 根据实时加权使用率给内存安全的候选节点打 0–10 分，负载越低分数越高。默认权重是 CPU 10%、内存 90%；动态评分遇到指标错误时会返回中性分。内存保护则会在无法读取 Metrics API 或集群 Pod 列表时拒绝本轮调度，以免在未知容量下放行 Pod。Metrics API 本地缓存时间为 5 秒。

测试前确认 Metrics Server 可用：

```bash
kubectl top nodes
```

无需给节点添加优先级标签。测试 Pod 必须声明正数的内存 request 或 limit；调度时按 request/limit 中较大的值计算。

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
      resources:
        requests:
          memory: 64Mi
        limits:
          memory: 128Mi
```

检查：

```bash
kubectl get pod scheduler-extender-test -o wide
kubectl describe pod scheduler-extender-test
kubectl -n kube-system logs deployment/i-scheduler-extender -c i-scheduler-extender --tail=100
```

### 5.3 修改策略

修改并应用 `SchedulerPolicy/cluster`，controller 会自动加载新策略：

```bash
kubectl edit schedulerpolicy cluster
kubectl get schedulerpolicy cluster -o yaml
```

只有进程监听或 manager 启动选项变更时才需要修改 [scheduler.yaml](../config/manager/scheduler.yaml) 并重启 manager。组合清单使用相同的 CRD 和 `SchedulerPolicy/cluster` API；不需要修改控制面节点上的 `/etc/kubernetes/manifests/kube-scheduler.yaml`。

## 6. 方式二：集成已有 kube-scheduler

这种方式只部署 Extender，已有 kube-scheduler 通过 Service 调用它。先确保 CRD 和策略对象已安装，再部署 Extender：

```bash
kubectl apply -f config/crd/bases/scheduler.example.com_schedulerpolicies.yaml
kubectl wait --for=condition=Established --timeout=60s crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
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
core/pods             get, list
core/pods/binding     create
scheduler.example.com/schedulerpolicies get, list, watch, update, patch
scheduler.example.com/schedulerpolicies/status get, update, patch
metrics.k8s.io/nodes  get, list
```

先安装 CRD 并创建 `SchedulerPolicy/cluster`，再启动任一 manager 部署方式。`manifest.yaml` 为了示例快速运行使用了 `cluster-admin`，生产环境应拆分 ServiceAccount 并收紧权限。`manifest-integrated.yaml` 的 ClusterRole 按当前代码授予上述权限：读取 Metrics API、统计 Pod 内存预算、维护 Node 内存保护标签，以及执行 Bind。

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
    -> 超过 highThreshold 的节点执行 cordon
    -> 至少保留 minSchedulableNodes 个可调度节点
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
- `topNodesCount`、`minSchedulableNodes` 与节点总数匹配；
- 已设置节点白名单，避免节点保护逻辑处理控制平面或特殊用途节点；
- Extender ServiceAccount 可以读取 Metrics API；
- 工作负载设置了内存 request 或 limit；
- 已确认 `scheduler.example.com/memory-blocked` 标签由 Extender 管理，且 75% 恢复阈值符合预期；
- 已确认 cordon 不会驱逐已有 Pod；
- 已将 `cluster-admin` 替换为审核后的最小 RBAC；
- 已验证 scheduler、Extender、Metrics API Server 之间的网络连通性；
- 已准备参数、镜像和 scheduler 配置的回滚方案。
