# 说明

最终部署方案请优先阅读 [DEPLOYMENT-STARTUP.md](DEPLOYMENT-STARTUP.md)。本文保留为“复用已有 kube-scheduler”的补充说明；节点保护策略由集群级 `SchedulerPolicy/cluster` 自定义资源管理，挂载的 YAML 只保存 manager 进程参数。

# i-scheduler-extender 集成到现有 kube-scheduler

## 1. 为什么要改成这种部署方式

当前 `manifest.yaml` 会在一个 Deployment 中额外启动一个 `kube-scheduler`，并创建一个名为 `i-scheduler-extender` 的调度器 profile。因此业务 Pod 必须显式配置：

```yaml
spec:
  schedulerName: i-scheduler-extender
```

Scheduler Extender 本身只是 HTTP 服务，不能脱离 kube-scheduler 工作。推荐的改造方式是：

1. 只部署 `i-scheduler-extender` HTTP 服务；
2. 修改集群已经运行的 kube-scheduler 配置，在其中增加 extender；
3. 使用现有默认 profile `default-scheduler`。

这样不需要额外运行一个 kube-scheduler，业务 Pod 也不需要再写 `schedulerName`。所有经过默认调度器的 Pod 都会经过该 extender 的 Filter、Prioritize 和 Bind 阶段，请先确认这是期望的行为。

默认 scheduler 接入 Extender 前，必须为 Operator 配置启动旁路，确保 `Recreate` 升级时新实例可以启动。推荐使用 Kubebuilder 的 [config/bootstrap](../config/bootstrap/kustomization.yaml) 入口，根据实际节点修改 patch 中的 `nodeName`；当前服务器为 `m1`。`nodeName` 使 kubelet 直接接管 Pod，固定运行节点。kube-scheduler 的 `extenders` 在所有 profile 间共用，因此新增 profile 不能绕过服务。完整操作见 [单实例接管和启动旁路](DEPLOYMENT-STARTUP.md#单实例接管和启动旁路)。

## 2. 前置条件

- 具有修改控制平面 kube-scheduler 配置的权限；
- 能够在集群中部署一个 HTTP 服务；
- Metrics Server 正常运行，`kubectl top nodes` 可以返回数据；
- extender 镜像已经构建并推送到集群可访问的镜像仓库。

构建镜像示例：

```bash
cd /data/scheduler/i-scheduler-extender
make build-image
```

如果 Makefile 中的镜像地址不是当前集群使用的仓库，请修改 `Makefile` 的 `IMG`，重新构建并推送。

## 3. 部署 extender 服务

不要直接使用当前的 `manifest.yaml`，因为它会再次启动一个独立的 kube-scheduler。先创建下面的文件，例如保存为 `deploy/manifest-integrated.yaml`，并把 `image` 替换成实际的 extender 镜像。

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: i-scheduler-extender
  namespace: kube-system
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: i-scheduler-extender-config
  namespace: kube-system
data:
  config.yaml: |
    metricsBindAddress: "0"
    healthProbeBindAddress: ":8082"
    extenderBindAddress: ":8080"
    leaderElection: true
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: i-scheduler-extender
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch", "patch", "update"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/binding"]
    verbs: ["create"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["nodes"]
    verbs: ["get", "list"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: i-scheduler-extender
subjects:
  - kind: ServiceAccount
    name: i-scheduler-extender
    namespace: kube-system
roleRef:
  kind: ClusterRole
  name: i-scheduler-extender
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: v1
kind: Service
metadata:
  name: i-scheduler-extender
  namespace: kube-system
spec:
  selector:
    component: i-scheduler-extender
    tier: control-plane
  ports:
    - name: http
      port: 8080
      targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: i-scheduler-extender
  namespace: kube-system
  labels:
    component: i-scheduler-extender
    tier: control-plane
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      component: i-scheduler-extender
      tier: control-plane
  template:
    metadata:
      labels:
        component: i-scheduler-extender
        tier: control-plane
    spec:
      serviceAccountName: i-scheduler-extender
      containers:
        - name: i-scheduler-extender
          image: lixd96/i-scheduler-extender:v1
          imagePullPolicy: IfNotPresent
          args:
            - --config=/etc/i-scheduler-extender/config.yaml
          ports:
            - name: http
              containerPort: 8080
            - name: health
              containerPort: 8082
          volumeMounts:
            - name: operator-config
              mountPath: /etc/i-scheduler-extender/config.yaml
              subPath: config.yaml
              readOnly: true
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            initialDelaySeconds: 3
          livenessProbe:
            httpGet:
              path: /healthz
              port: health
            initialDelaySeconds: 10
      volumes:
        - name: operator-config
          configMap:
            name: i-scheduler-extender-config
```

先安装 CRD、创建集群策略，再部署 manager：

```bash
kubectl apply -f config/crd/bases/scheduler.example.com_schedulerpolicies.yaml
kubectl wait --for=condition=Established --timeout=60s crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
kubectl apply -f deploy/manifest-integrated.yaml
kubectl -n kube-system rollout status deploy/i-scheduler-extender
kubectl -n kube-system get pod,svc i-scheduler-extender
```

ClusterRole 包含 controller 和 Extender 当前所需权限：监听、读取和更新 Node，监听 `schedulerpolicies.scheduler.example.com` 并更新其 status，列出 Pod 统计已分配内存预算，跨命名空间创建 `pods/binding`，读取 `metrics.k8s.io/nodes`，并使用 Lease 进行 controller leader election。策略修改通过 `kubectl edit schedulerpolicy cluster` 完成；controller 会自动协调新设置。实际生产权限仍应按集群策略审核。

## 4. 修改现有 kube-scheduler 配置

### 4.1 先确认 kube-scheduler 的启动方式

在控制平面节点执行：

```bash
kubectl -n kube-system get pod -l component=kube-scheduler -o wide
sudo grep -n -- '--config\|kube-scheduler' /etc/kubernetes/manifests/kube-scheduler.yaml
```

找到 `kube-scheduler` 的 `--config` 参数。常见情况是 kubeadm 静态 Pod 使用 `/etc/kubernetes/scheduler-config.yaml` 或类似路径。不要覆盖原配置；在原 `KubeSchedulerConfiguration` 中合并下面的 `extenders` 配置，并保留原有的 leader election、认证、TLS 和其他 profiles 设置。

### 4.2 增加 extender 配置

在现有 `KubeSchedulerConfiguration` 中增加或合并：

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: default-scheduler
extenders:
  - urlPrefix: "http://i-scheduler-extender-http.i-scheduler-extender-system.svc.cluster.local:8080"
    enableHTTPS: false
    filterVerb: "filter"
    prioritizeVerb: "prioritize"
    bindVerb: "bind"
    nodeCacheCapable: false
    weight: 1
```

注意：

- Kubernetes 1.22 使用 `kubescheduler.config.k8s.io/v1beta2`；较新的版本通常使用 `v1`。必须以当前 kube-scheduler 支持的版本为准；
- 不要把 profile 名称写成 `i-scheduler-extender`，否则仍然需要业务 Pod 指定 `schedulerName`；
- 不要把现有的 `leaderElect: true` 改成 `false`。原有 leader election 配置应保持不变；
- 如果现有配置已有 `profiles`，只需确保默认 profile 的 `schedulerName` 是 `default-scheduler`，不要重复添加同名 profile；
- kube-scheduler 必须能够访问 `i-scheduler-extender-http.i-scheduler-extender-system.svc.cluster.local:8080`。如果控制平面网络不能访问 Service DNS，应改用控制平面可达的 extender 地址，并同步调整 `urlPrefix`。

### 4.3 让 kube-scheduler 重新加载配置

如果是 kubeadm 静态 Pod，需要让 `--config` 指向调度配置，并通过 `hostPath` 将文件挂进容器。控制平面两份文件的路径、挂载和 DNS 设置见 [控制平面需要修改的两份配置](DEPLOYMENT-STARTUP.md#控制平面需要修改的两份配置)。这些文件由部署操作配置，Operator 和 `SchedulerPolicy` 不会自动修改它们。

仅保存挂载的调度配置文件不会触发 kubelet 重建，kube-scheduler 也不会自动重新加载。修改静态 Pod 清单的实际内容（例如更新配置版本 annotation）后，kubelet 才会重建调度器并加载新配置。备份文件应放在 `/etc/kubernetes/manifests` 之外；仅删除 API 中的镜像 Pod 不会重启静态 Pod 容器。可以观察：

```bash
kubectl -n kube-system get pod -l component=kube-scheduler -w
kubectl -n kube-system logs -l component=kube-scheduler --tail=100
```

确认日志中没有配置解析错误，并检查 extender 是否收到请求：

```bash
kubectl -n kube-system logs deploy/i-scheduler-extender --tail=100
```

## 5. 业务 Deployment 的写法

改造后删除 `schedulerName`，使用 Kubernetes 默认值即可：

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
        - name: nginx
          image: busybox:1.36
          command: ["sleep"]
          args: ["99999"]
          resources:
            requests:
              memory: 64Mi
            limits:
              memory: 128Mi
```

部署测试：

```bash
kubectl apply -f deploy/deploy-test.yaml
kubectl get pod -o wide
kubectl describe pod -l app=test
```

调度打分按节点实时加权 CPU、内存使用率计算，不需要给节点添加优先级标签。Filter/Bind 使用实测内存、尚未被采样覆盖的预约及新 Pod 预算；预算优先用正数 request，没有正数 request 时回退 limit。预计占用超过 80% 时标记并排除节点；当前值加预约降至 75% 以下且新 Pod 不会突破 80% 时解除标签。部署前确认 `kubectl top nodes` 正常，ServiceAccount 可以读取 `metrics.k8s.io/nodes` 和集群 Pod 列表。

80% 是基于 Metrics API 实测内存和新 Pod 预算的调度预测门槛，不是节点物理内存的绝对上限。request 小于 limit 时，新 Pod 的增量按 request 计算，因此保留超卖空间；没有正数 request 时使用 limit。指标有采样延迟，Pod 可能超过 request（特别是没有 limit 时），未经过此 Extender 的调度路径也不会被它拦截。`scheduler.example.com/memory-blocked=true` 标签用于识别和观测，实际调度拦截由 Extender 的 Filter/Bind 执行。

内存预约按 Pod UID 保存在 leader 进程内，设置 nodeName 不会立即释放。应用容器及可重启 init 容器启动后，新的 NodeMetrics 完整窗口覆盖启动时间才释放；启动等待无固定清账 TTL，删除、终止或 UID 替换时清理。新 leader 在开放 HTTP 前重建过渡期预算。指标须有有效 timestamp/window 且不超过 2 分钟，过旧或无效指标阻止节点准入。

保持 `replicas: 1` 和 `Recreate`。leader 初始化完成且 HTTP 开始监听后才 Ready，升级期间依赖 Extender 的新调度会暂停。使用本文旧清单时，部署前在 `spec.template.spec` 添加实际可用节点的 `nodeName`，并保留到后续升级；使用 Kubebuilder manager 时通过 `config/bootstrap` 入口配置旁路。

正常情况下，Pod 的 `Scheduled` 事件来源会显示默认调度器，但调度请求也会出现在 extender 日志中；调度结果仍受该 extender 的 Filter/Prioritize/Bind 逻辑影响。

## 6. 回滚

1. 从现有 kube-scheduler 配置中删除 `extenders` 配置，等待静态 Pod 恢复；
2. 删除集成部署的资源：

   ```bash
   kubectl delete -f deploy/manifest-integrated.yaml
   ```

3. 如果业务 YAML 曾经写入 `schedulerName: i-scheduler-extender`，需要删除该字段或恢复原来的调度器配置。

## 7. 方案边界

这种方式的核心变化是“复用集群已有的 kube-scheduler”，不是“完全不需要 kube-scheduler”。Kubernetes 的 Pod 调度始终需要 kube-scheduler；如果没有权限修改控制平面上的 kube-scheduler 配置，只能继续使用原来的独立调度器方案，或者改为实现 Kubernetes Scheduler Framework 插件并安装到控制平面镜像中。


## 8. ACK 托管集群说明

### 8.1 普通 ACK 托管控制面

如果使用的是 ACK 托管版集群，控制面由阿里云管理，通常无法访问或修改：

```text
/etc/kubernetes/manifests/kube-scheduler.yaml
```

这种情况下不能按本文第 4 节把 extender 注入 ACK 内置的 kube-scheduler；`manifest-integrated.yaml` 只能部署 HTTP 服务，不能让托管调度器自动加载它。也不要尝试在业务节点上创建同名的 `default-scheduler`，这会与 ACK 内置调度器产生竞争，风险很大。

ACK 托管集群建议按下面的优先级选择方案：

1. **优先使用 ACK/阿里云已支持的调度扩展能力**。如果当前 ACK 版本和集群规格提供调度插件、调度策略或调度器扩展能力，按照 ACK 控制台和对应版本文档配置；这类能力由 ACK 负责修改或管理控制面，不能直接套用本项目的 legacy Scheduler Extender 配置。
2. **将逻辑改造成 Kubernetes 原生调度约束**。简单的硬约束可使用 `nodeSelector`、`nodeAffinity`、拓扑约束等字段，通常不需要额外调度器。动态资源负载评分仍需要相应的调度扩展能力。
3. **运行独立的自定义 kube-scheduler**。如果必须继续使用本项目的 Scheduler Extender，就在 ACK 中以普通 Deployment 运行自定义调度器，并使用独立的 scheduler name；这是托管控制面无法修改时最通用的方案。

### 8.2 ACK 中部署独立调度器

ACK 中的部署关系如下：

```text
业务 Pod --schedulerName=i-scheduler-extender--> 自定义 kube-scheduler
                                                       |
                                                       +--> i-scheduler-extender Service
                                                       +--> ACK API Server
```

可以基于原来的 `manifest.yaml` 部署，但需要注意：

- `kube-scheduler` 镜像版本应与 ACK 集群 Kubernetes 主版本兼容，不要固定沿用 `v1.22.9`；
- 自定义 scheduler 的配置继续使用 `schedulerName: i-scheduler-extender`；
- 业务 Pod 仍需指定 `schedulerName: i-scheduler-extender`；
- 自定义 scheduler 和 extender 都必须使用 ACK 集群可用的 ServiceAccount/RBAC，不能假设控制面节点上的 `/etc/kubernetes/scheduler.conf` 存在；
- 使用专用 ServiceAccount 的 in-cluster 配置连接 ACK API Server，不要把管理员 kubeconfig 或访问凭据写入镜像；
- 原 `manifest.yaml` 中的 `cluster-admin` 只是为了示例快速运行，生产环境应按实际 API 请求收敛权限。

### 8.3 不想在每个业务 YAML 中写 schedulerName

在 ACK 无法修改默认 kube-scheduler 的前提下，不能仅靠自定义 scheduler 配置自动接管未指定 `schedulerName` 的 Pod。可以增加 Mutating Admission Webhook，在 Pod 创建时为指定命名空间或带指定标签的 Pod 自动注入：

```yaml
spec:
  schedulerName: i-scheduler-extender
```

这种方式只是把配置从业务 YAML 移到了准入层，底层仍然需要独立的自定义 kube-scheduler。生产使用时还要考虑 Webhook TLS、可用性、`failurePolicy`、排除 `kube-system` 和 Webhook 自身 Pod，以及升级和回滚策略。若只是少量业务，直接写 `schedulerName` 通常更容易运维。

### 8.4 ACK 部署结论

| ACK 场景 | 能否修改内置 kube-scheduler | 推荐方式 |
| --- | --- | --- |
| 普通托管控制面 | 通常不能 | ACK 原生调度能力；或独立自定义 scheduler + 独立 schedulerName |
| 具备 ACK 调度扩展能力的集群 | 由 ACK 能力决定 | 按对应版本文档使用官方调度扩展机制 |
| 自建/专有控制面且有控制面节点权限 | 可以 | 使用本文前面的集成方案，复用现有 kube-scheduler |
