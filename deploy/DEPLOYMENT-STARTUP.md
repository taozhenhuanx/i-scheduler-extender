# i-scheduler-extender Operator 部署文档

## 1. 部署方案与文件入口

当前分支使用 Kubebuilder/controller-runtime manager 运行 Operator，集群级 `SchedulerPolicy/cluster` 管理调度权重、阈值和节点保护策略。同一 manager 提供 Scheduler Extender HTTP 接口，由集群已有的默认 kube-scheduler 调用。接入完成后，业务 Pod 省略 `schedulerName` 即经过 `/filter`、`/prioritize` 和 `/bind`。

Operator 的 Namespace、CRD、RBAC、ConfigMap、Deployment 和 Service 统一由 `config/` 下的 Kustomize 配置维护。`deploy/` 保存当前方案的说明、控制平面配置示例和验证 Pod：

| 文件 | 用途 |
|---|---|
| [DEPLOYMENT-STARTUP.md](DEPLOYMENT-STARTUP.md) | 构建、安装、接入默认调度器、升级和卸载 |
| [NODEBALANCER-CONFIG.md](NODEBALANCER-CONFIG.md) | SchedulerPolicy 字段和节点管理规则 |
| [SCHEDULING-LOGIC.md](SCHEDULING-LOGIC.md) | 调度逻辑和功能验证 |
| [DEPLOYMENT-20261020.md](DEPLOYMENT-20261020.md) | 当前服务器的部署、扩容和验证记录 |
| [kube-scheduler-config-v1beta2.yaml](kube-scheduler-config-v1beta2.yaml) | Kubernetes 1.22 的默认调度器配置示例 |
| [kube-scheduler-static-pod.yaml](kube-scheduler-static-pod.yaml) | 默认调度器的启动参数、挂载和 DNS 示例 |
| [scheduler-extender-check-local.yaml](scheduler-extender-check-local.yaml) | 使用当前节点缓存镜像的验证 Pod |

详细准入、实测打分和预留释放流程见 [调度逻辑](SCHEDULING-LOGIC.md)。80% 内存门槛是采样与预算计算出的调度预测值；运行中的 Pod 可能超过 request，实际占用仍可能越过门槛。

## 2. 前置条件与镜像构建

- 可以访问集群 API Server，并具备修改控制平面 kube-scheduler 配置的权限。
- Metrics Server 正常，`kubectl top nodes` 可以返回指标。
- Operator 运行节点 Ready、有资源余量，且没有磁盘或内存压力。
- 所有可能运行 Operator 的节点能取得相应架构的镜像。

在仓库根目录执行：

```bash
kubectl config current-context
kubectl get nodes
kubectl top nodes
make docker-build IMG=registry.example.com/platform/i-scheduler-extender:v1
make docker-push IMG=registry.example.com/platform/i-scheduler-extender:v1
```

需要构建并推送多架构镜像时，使用 `make build-image IMG=...`。当前单节点 Docker 集群也可使用节点本机已有镜像，Deployment 的 `imagePullPolicy` 为 `IfNotPresent`；本机缓存不会自动分发到其他节点。本次实际镜像构建方式见 [部署记录](DEPLOYMENT-20261020.md#镜像构建方式)。

## 3. 进程配置与 SchedulerPolicy

进程配置位于 [config/manager/scheduler.yaml](../config/manager/scheduler.yaml)，Kustomize 将它生成 ConfigMap：

```yaml
metricsBindAddress: "0"
healthProbeBindAddress: ":8082"
extenderBindAddress: ":8080"
leaderElection: true
```

安装 CRD 并创建策略：

```bash
make install
kubectl wait --for=condition=Established --timeout=60s crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
```

策略样例使用 CPU/内存权重 `10/90`、高负载/恢复阈值 `70/65`、最少可用节点 `4`、Top N `9`、周期 `5` 秒，`nodeSelector: {}` 管理所有节点。根据实际集群调整管理范围和下限，字段含义见 [策略配置说明](NODEBALANCER-CONFIG.md)。单节点设置下限为 4 时，不会新增自动 cordon；Extender 内存准入独立工作。

更新现有策略时，编辑 CR 或应用自己的策略文件，避免用默认样例覆盖已定制的值：

```bash
kubectl edit schedulerpolicy cluster
kubectl get schedulerpolicy cluster -o yaml
```

策略修改由 controller 自动协调。进程配置修改则需要重启 Operator，后续操作见第 7 节。

## 4. 安装 Operator

### 单实例接管和启动旁路

保持 `replicas: 1` 和 `Recreate`。只有 leader 加载策略、重建尚未被指标覆盖的 Pod 预留并开始监听 HTTP 后，`/readyz` 才通过。升级时旧实例先退出，新实例再初始化；期间依赖 Extender 的新调度会暂停，已有业务容器继续运行。

在首次接入默认 scheduler 前，为 Operator 配置启动旁路并在升级中保留。否则新 Operator Pod 会依赖自身尚未启动的 HTTP 服务完成调度。`config/bootstrap` 使用 `nodeName`，由 kubelet 直接接管 Operator；额外 scheduler profile 不能绕过 kube-scheduler 全局共用的 `extenders`。

先修改 [manager_bootstrap_patch.yaml](../config/bootstrap/manager_bootstrap_patch.yaml) 的 `nodeName`，当前服务器为 `m1`。确认节点可用后执行：

```bash
kubectl get nodes
kubectl kustomize config/bootstrap > /tmp/i-scheduler-extender-bootstrap.yaml
# 检查渲染后的 nodeName 和 Recreate，再设置实际镜像进行部署。
make deploy-bootstrap IMG=registry.example.com/platform/i-scheduler-extender:v1
kubectl -n i-scheduler-extender-system rollout status deployment/i-scheduler-extender-controller-manager --timeout=180s
kubectl -n i-scheduler-extender-system get pods -o wide
kubectl -n i-scheduler-extender-system get endpoints i-scheduler-extender-http
```

`make deploy-bootstrap` 包含 CRD、RBAC、manager 和 Service。`make deploy` 使用 `config/default`，不包含启动旁路；当前默认调度器接入方案的安装与升级使用 bootstrap 入口。

`nodeName` 固定运行节点，该节点失效时需要调整 patch；它不会绕过 kubelet 的磁盘压力准入。当前单节点使用此方案，多节点部署时需要明确 Operator 的可用运行节点。

## 5. 接入默认 kube-scheduler

### 控制平面需要修改的两份配置

当前服务器采用 kubeadm 静态 Pod，需要在控制平面节点修改以下两份文件：

| 服务器文件 | 修改内容 |
|---|---|
| `/etc/kubernetes/scheduler-config.yaml` | 保留 `default-scheduler` profile，增加 Extender 地址及三个接口 |
| `/etc/kubernetes/manifests/kube-scheduler.yaml` | 添加 `--config` 和文件挂载，设置集群 DNS 与 `ndots: "1"` |

修改前备份两份文件，静态 Pod 的备份放在 `/etc/kubernetes/manifests` 之外。在现有 `KubeSchedulerConfiguration` 中合并以下字段，保留原有的认证、leader election 和其他 profiles：

```yaml
profiles:
  - schedulerName: default-scheduler
extenders:
  - urlPrefix: "http://i-scheduler-extender-http.i-scheduler-extender-system.svc.cluster.local:8080"
    enableHTTPS: false
    filterVerb: filter
    prioritizeVerb: prioritize
    bindVerb: bind
    nodeCacheCapable: false
    weight: 1
```

配置 API 版本必须与 kube-scheduler 兼容。当前 Kubernetes 1.22 使用 [v1beta2 示例](kube-scheduler-config-v1beta2.yaml)；其他版本按实际支持的配置 API 调整。

在已有静态 Pod 中合并以下字段，保留其他启动参数和挂载。完整示例见 [kube-scheduler-static-pod.yaml](kube-scheduler-static-pod.yaml)：

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
  dnsPolicy: ClusterFirstWithHostNet
  dnsConfig:
    options:
      - name: ndots
        value: "1"
  volumes:
    - name: scheduler-config
      hostPath:
        path: /etc/kubernetes/scheduler-config.yaml
        type: File
```

当前 kube-scheduler 使用 hostNetwork，`ClusterFirstWithHostNet` 使容器使用集群 DNS。宿主机搜索域包含 `localdomain`，默认 `ndots:5` 会先展开 Extender 域名，错误后缀可能转发到超时的外部 DNS。`ndots:1` 让带点的完整 Service 名称优先直接查询。

这些是部署时的控制平面配置操作。`make deploy`、`make deploy-bootstrap` 和 `SchedulerPolicy` CR 都不会自动修改这两份服务器文件。新集群需要完成接入后，省略 `schedulerName` 的业务 Pod 才会经过 Extender。

### 重新加载配置

修改静态 Pod 清单的实际内容后，kubelet 会重建 kube-scheduler。**仅修改挂载的 `scheduler-config.yaml` 不会触发重建，kube-scheduler 也不会自动重新加载配置**；后续单独更新它时，需要修改静态 Pod 的配置版本 annotation 等内容触发重建。仅删除 API 中的镜像 Pod 不会重启静态 Pod 容器。

```bash
kubectl -n kube-system get pods -l component=kube-scheduler -o wide
kubectl -n kube-system logs -l component=kube-scheduler --tail=100
```

确认配置解析正常、调度器 Ready，并核对容器实际 `resolv.conf` 的集群 DNS 地址与 `ndots:1`。本次实际修改及结果见 [部署记录](DEPLOYMENT-20261020.md)。

托管控制面通常无法修改这些文件，仅安装 Operator 不会让其默认调度器自动调用 Extender。此部署流程需要可配置 kube-scheduler 的控制平面。

## 6. 健康检查与调度验证

```bash
kubectl -n i-scheduler-extender-system get pods -o wide
kubectl -n i-scheduler-extender-system get endpoints i-scheduler-extender-http
kubectl get schedulerpolicy cluster -o yaml
kubectl top nodes
kubectl -n i-scheduler-extender-system logs deployment/i-scheduler-extender-controller-manager -c manager --since=10m --tail=100
```

| 信号 | 含义 |
|---|---|
| Operator `:8082/healthz` | 进程健康 |
| Operator `:8082/readyz` | leader 完成初始化且 Extender 已监听 |
| Extender `:8080/health` | HTTP 服务返回 `{"status":"UP"}` |
| CR `Ready=True` | 最近一次策略协调完整成功，确认 observedGeneration 与 generation 一致 |

当前服务器已缓存 Alpine 镜像，可以创建验证 Pod：

```bash
kubectl apply -f deploy/scheduler-extender-check-local.yaml
kubectl get pod scheduler-extender-check -o wide
kubectl describe pod scheduler-extender-check
kubectl -n i-scheduler-extender-system logs deployment/i-scheduler-extender-controller-manager -c manager --since=5m --tail=100
```

该 Pod 省略 `schedulerName` 和 `nodeName`。结合默认调度器配置、Pod 的 `Scheduled` 事件和 Operator 的 `bound pod` 日志确认完整链路。镜像缓存示例只适用于已缓存该镜像的节点，其他集群的通用示例和内存门槛验证见 [调度验证](SCHEDULING-LOGIC.md#6-在当前本机集群验证)。完成后删除验证 Pod：

```bash
kubectl delete pod scheduler-extender-check
```

## 7. 升级、回滚和卸载

升级时保留 bootstrap patch 和单实例 `Recreate`，使用实际新镜像：

```bash
make deploy-bootstrap IMG=registry.example.com/platform/i-scheduler-extender:v2
kubectl -n i-scheduler-extender-system rollout status deployment/i-scheduler-extender-controller-manager --timeout=180s
```

修改进程配置后，可重新部署并重启 manager，使其加载更新后的 ConfigMap：

```bash
make deploy-bootstrap IMG=registry.example.com/platform/i-scheduler-extender:v2
kubectl -n i-scheduler-extender-system rollout restart deployment/i-scheduler-extender-controller-manager
kubectl -n i-scheduler-extender-system rollout status deployment/i-scheduler-extender-controller-manager --timeout=180s
```

镜像回滚时，继续使用 bootstrap 入口指定上一版兼容镜像，避免恢复没有启动旁路的历史 Pod 模板：

```bash
make deploy-bootstrap IMG=registry.example.com/platform/i-scheduler-extender:v1
```

卸载时先从默认 kube-scheduler 配置移除本项目的 `extenders`，触发静态 Pod 重建，确认默认调度器恢复原生调度。随后删除策略，等待 controller 清理其自动 cordon 并完成 finalizer，再删除 Operator：

```bash
kubectl delete schedulerpolicy cluster --timeout=120s
make undeploy
```

策略删除若因节点清理失败而等待，检查 controller 日志和 CR 状态，保持 Operator 运行以便重试。`make undeploy` 会删除 CRD 等安装资源；先确认策略已完成清理。

## 8. RBAC 与节点保护

权限由 [config/rbac](../config/rbac) 管理，包括读取和更新 Node、读取 Pod、创建 `pods/binding`、读取 Metrics API、协调 SchedulerPolicy 及其 status，以及通过 Lease 进行 leader election。

NodeBalancer 周期清理退出管理范围的自动 cordon，读取新鲜指标，先恢复负载回落的自动 cordon，再对受管可用节点的 Top N 高负载候选考虑新增 cordon。新增动作不得主动将可用节点减到 `minSchedulableNodes` 以下；不会驱逐已有 Pod，人工 cordon 保留。

CR 的 Ready 反映策略协调结果，HTTP Ready 反映调度服务是否可接收请求。管理员接管自动 cordon、指标失败和可用节点计数规则见 [策略配置说明](NODEBALANCER-CONFIG.md)。
