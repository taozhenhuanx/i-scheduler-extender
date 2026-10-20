# i-scheduler-extender Operator 部署文档

## 1. 方案说明

进程由 Kubebuilder/controller-runtime manager 管理。集群级 `SchedulerPolicy/cluster` 是调度策略的期望状态；controller 监听策略和 Node 变化，并按 CR 的 spec 协调节点负载保护和 Scheduler Extender 的打分权重。同一 manager 还托管 Scheduler Extender HTTP 接口。调度打分和节点保护通过 `metrics.k8s.io/v1beta1` 读取节点实际 CPU 和内存使用率。Filter/Bind 使用节点实测内存、尚未被采样覆盖的预约及新 Pod 预算；新 Pod 优先使用正数 request，没有正数 request 时回退到 limit。预计占用超过 Allocatable 的 80% 时标记并排除节点；当前值加预约降至 75% 以下且新 Pod 不会突破 80% 时解除标记。

80% 是调度预测门槛，不是物理内存绝对上限：Metrics API 存在采样延迟，Pod 可能超过 request，且未经过此 Extender 的调度路径不受拦截。实际拒绝由 Extender 的 Filter/Bind 执行。

项目提供三种部署方式：

- `make deploy`：通过 Kubebuilder 的 Kustomize 配置安装 CRD、manager 和 Extender Service；首次部署还需要创建 `SchedulerPolicy/cluster`。
- `manifest.yaml`：在一个 Deployment 中运行自定义 `kube-scheduler` 和 Extender。业务 Pod 需要设置 `schedulerName: i-scheduler-extender`。
- `manifest-integrated.yaml`：只运行 Extender，复用已有 kube-scheduler。需要把 Extender 配置加入已有 scheduler，业务 Pod 使用默认 scheduler。

### 单实例接管和启动旁路

保持 `replicas: 1` 和 `Recreate`。只有 leader 在完成当前策略加载、重建过渡期内存预算并开始监听 HTTP 后，`/readyz` 才返回成功；standby 不监听调度端口。升级时旧 Pod 先退出，新 leader 再初始化并接管，期间依赖 Extender 的新调度会暂停。

若默认 kube-scheduler 已接入此 Extender，新 Operator Pod 也会依赖该服务完成调度。必须在首次接入默认 scheduler 前为 Operator 配置启动旁路，并在后续升级中保留。`config/bootstrap` 在 Pod 中指定 `nodeName`，使 kubelet 直接接管；`extenders` 是 kube-scheduler 所有 profile 共用的配置，单纯新增一个 profile 不能绕过它。

当前单节点服务器使用 `m1`。在其他集群部署前，先修改 [manager_bootstrap_patch.yaml](../config/bootstrap/manager_bootstrap_patch.yaml) 中的 `nodeName` 为实际可用节点，确认该节点有运行 Operator 的容量，然后渲染部署：

```bash
kubectl get nodes
# 修改 patch 中的节点名；渲染检查 nodeName 和 Recreate。
kubectl kustomize config/bootstrap > /tmp/i-scheduler-extender-bootstrap.yaml
# 镜像构建并推送后，设置实际镜像地址并部署同一个 bootstrap 入口。
make deploy-bootstrap IMG=registry.example.com/platform/i-scheduler-extender:v1
kubectl -n i-scheduler-extender-system rollout status deployment/i-scheduler-extender-controller-manager
```

此入口包含 CRD、RBAC、manager 和 Service；首次安装仍需创建 `SchedulerPolicy/cluster`。采用此入口后，升级继续使用 `make deploy-bootstrap`；`make deploy` 使用 `config/default`，不会自动包含节点旁路。`nodeName` 会固定 Operator 的运行节点并绕过 scheduler 的节点选择，节点失效时需修改该字段；该方式适合当前单节点部署。

使用旧的 `deploy/manifest-integrated.yaml` 时，需要在其 Deployment 的 `spec.template.spec` 添加同样的 `nodeName` 后部署。独立 scheduler 的组合清单可由未接入 Extender 的默认 scheduler 启动。

### 内存预算接管

Bind 前按 Pod UID 登记内存预算，设置 `nodeName` 后继续保留。所有应用容器与可重启 init 容器启动，且 NodeMetrics 的完整采样窗口覆盖其启动时间后才释放；启动等待不使用固定 TTL 清账。Pod 删除、终止或 UID 替换时清理旧预约。新 leader 在监听 HTTP 前重建未被采样覆盖的已绑定 Pod 预算，稳定 Pod 使用实测内存。

内存准入要求指标包含有效 `timestamp` 和 `window`，样本不超过 2 分钟，最多允许 10 秒的未来时钟偏差。过旧、无效或缺失指标会阻止对应节点准入。Bind 超时或冲突时保留预约，同一 Pod 只重试原节点，避免结果不确定时丢失旧节点预算。

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

`spec.topNodesCount: 9` 是硬限制：程序将受管可用节点按综合使用率从高到低排序，只对前 9 个节点做高负载 cordon 判断。无效指标或故障节点不占名额；该排序的第 10 个节点即使使用率达到 95%，也不会因为本轮 Top N 规则被新 cordon。

如果希望所有受管节点都有机会参与处理，应将它设置为不小于白名单节点数量。`spec.minSchedulableNodes` 阻止新增 cordon 将受管可用节点降到下限以下；可用节点须 Ready、未删除、未 cordon、指标有效且新鲜，且无内存保护标记及 `NoSchedule`/`NoExecute` taint。已有故障导致数量不足时停止新增 cordon。

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

无需给节点添加优先级标签。测试 Pod 必须声明正数的内存 request 或 limit；调度预算优先按 request 计算，未设置正数 request 时使用 limit。

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
  - urlPrefix: "http://i-scheduler-extender-http.i-scheduler-extender-system.svc.cluster.local:8080"
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
- kube-scheduler 必须能访问 Extender Service DNS；Kubebuilder 部署的 Service 地址是 `i-scheduler-extender-http.i-scheduler-extender-system.svc.cluster.local:8080`；
- 集成方式下业务 Pod 不需要设置 `schedulerName`。

当前集群运行 Kubernetes 1.22.9，可使用 [`kube-scheduler-config-v1beta2.yaml`](kube-scheduler-config-v1beta2.yaml) 中的组件配置片段。将 `profiles` 和 `extenders` 合并到现有 `KubeSchedulerConfiguration`，不要覆盖控制平面原有的认证、leader election 等设置。kube-scheduler 为 hostNetwork Pod 时应使用 `dnsPolicy: ClusterFirstWithHostNet`，以便解析 Service DNS。

### 控制平面需要修改的两份配置

当前服务器采用 kubeadm 静态 Pod。接入默认调度器需要在控制平面节点修改以下两份文件：

| 服务器文件 | 修改内容 |
|---|---|
| `/etc/kubernetes/scheduler-config.yaml` | 保留 `default-scheduler` profile，增加上述 `extenders`，让调度器调用 `/filter`、`/prioritize`、`/bind` |
| `/etc/kubernetes/manifests/kube-scheduler.yaml` | 添加 `--config` 启动参数和配置文件挂载；设置集群 DNS 策略及下方的 `ndots: "1"` |

在现有静态 Pod 清单中合并以下字段，保留其他启动参数和挂载。完整示例见 [kube-scheduler-static-pod.yaml](kube-scheduler-static-pod.yaml)：

```yaml
spec:
  containers:
    - name: kube-scheduler
      command:
        - kube-scheduler
        # 合并到已有 command 列表，保留其他参数。
        - --config=/etc/kubernetes/scheduler-config.yaml
      volumeMounts:
        - name: scheduler-config
          mountPath: /etc/kubernetes/scheduler-config.yaml
          readOnly: true
  volumes:
    - name: scheduler-config
      hostPath:
        path: /etc/kubernetes/scheduler-config.yaml
        type: File
```

这是部署时对控制平面进行的配置操作。`make deploy`、`make deploy-bootstrap` 和 Operator 的 `SchedulerPolicy` CR 都不会自动修改这两份服务器文件；CR 管理权重、阈值和节点保护策略。接入完成后，省略 `schedulerName` 的业务 Pod 使用 `default-scheduler`，因此会经过 Extender。新集群也需要完成这一接入步骤。

当前服务器的宿主机搜索域包含 `localdomain`。默认 `ndots:5` 会先展开 Extender 的 Service 域名，错误后缀查询可能被转发到不可达的外部 DNS，耗尽 Extender 请求超时。静态 Pod 示例使用以下设置，优先查询带点的完整域名：

```yaml
dnsPolicy: ClusterFirstWithHostNet
dnsConfig:
  options:
    - name: ndots
      value: "1"
```

修改前备份两份文件，将静态 Pod 的备份放在 `/etc/kubernetes/manifests` 之外。修改静态 Pod 清单的实际内容后，kubelet 会自动重建调度器，期间新调度会短暂停顿，已有业务容器继续运行。**仅修改挂载的 `scheduler-config.yaml` 不会触发重建，kube-scheduler 也不会自动重新加载该配置**；后续单独更新它时，还需要修改静态 Pod 的配置版本 annotation 等内容，触发重建。仅删除 API 中的镜像 Pod 不会重启静态 Pod 容器。

重建后检查容器实际 `resolv.conf`，确认集群 DNS 地址及 `ndots:1` 生效，并结合调度器日志和 Extender 的 `bound pod` 日志确认接入。当前服务器的实际改动和验证结果见 [本次部署记录](DEPLOYMENT-20261020.md)。

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
清理退出策略范围节点的自动 cordon 和过期标记
    -> 读取 Metrics API，排除缺失、无效或过期的指标
    -> CPU 10% + 内存 90% 计算综合使用率
    -> 按综合使用率降序排序
    -> 取 Top N
    -> 超过 highThreshold 的节点执行 cordon
    -> 新增 cordon 不得将受管可用节点降到 minSchedulableNodes 以下
```

程序通过补丁更新 Node 的调度状态和 `scheduler.example.com/auto-cordoned` 管理标记，例如：

```yaml
spec:
  unschedulable: true
```

不会驱逐已有 Pod。节点退出 `nodeSelector` 或加入 `ignoreNodes` 时，清理本控制器的自动 cordon；没有自动标记的人工 cordon 保留。管理员接管已自动 cordon 的节点时须先移除自动标记，操作见 [策略配置说明](NODEBALANCER-CONFIG.md#管理员接管节点)。

缺失/无效指标及 cordon/uncordon 操作失败会汇总到 CR 的 `Ready=False` 并触发重试，其他节点继续处理；后续完整成功时恢复 `Ready=True`。此状态反映策略协调结果，HTTP `/readyz` 反映 Extender 服务是否已完成初始化并监听。

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
