# Kubernetes Scheduler Operator and Extender

项目骨架由 Kubebuilder v4 初始化，使用 controller-runtime manager 运行 `SchedulerPolicy` 自定义资源对应的 controller，并提供 HTTP Scheduler Extender 接口。controller 监听集群级 `SchedulerPolicy/cluster` 和 Node 变化，根据 CR 的 `spec` 协调节点 cordon 状态及 Extender 打分权重；Extender 读取 `metrics.k8s.io` 的节点负载动态打分，并阻止预计会超过内存门槛的 Pod 落到该节点。

## 调度行为

- Filter 使用节点实测内存、尚未被采样覆盖的 Pod 预留和待调度 Pod 的内存预算估算调度后的内存占用；超过 Allocatable 的 80% 时给节点添加 `scheduler.example.com/memory-blocked=true` 并排除该节点。
- 节点实测内存加过渡期预留降到 75% 或以下，且新 Pod 放入后不超过 80% 时，Filter 会清除该标签并重新允许调度。
- Prioritize 按节点实际 CPU、内存使用量占 Allocatable 的比例计算加权负载。
- 分数为 `10 * (1 - 加权使用率 / 100)`，四舍五入并限制在 0–10；负载越低，分数越高。
- `SchedulerPolicy/cluster` 中的 `cpuWeight` 和 `memoryWeight` 同时控制调度打分和节点负载保护。字段是 0–100 的整数百分比，两者合计必须为 100，默认 10/90；支持显式 `0/100`、`100/0` 和 `1/99`。
- Metrics API 结果缓存 5 秒。内存保护所需的指标或 Pod 列表不可用时，Filter 会停止本轮调度以免在未知容量下放行 Pod；动态评分失败时则返回中性分 0。
- Bind 前登记以 Pod UID 标识的预算，绑定成功后继续保留。所有应用容器和可重启 init 容器启动后，只有 NodeMetrics 的完整采样窗口覆盖启动时间才释放预算；拉取镜像和启动等待期间不会因固定超时丢弃预算。Pod 删除、终止或 UID 替换后清理旧预约。
- Bind 超时或返回冲突时保留预约，同一 Pod 只重试原节点，防止结果不确定时覆盖旧节点预算；原节点一直不可用时需要删除并重建该 Pod。
- 新 Pod 必须声明正数的内存 request 或 limit；预算优先使用 request，没有正数 request 时回退到 limit，因此 request 小于 limit 时保留超卖空间。稳定运行且已经被指标覆盖的 Pod 使用实测内存。
- 内存准入要求 NodeMetrics 包含有效的 `timestamp` 和 `window`，样本距当前时间不超过 2 分钟，允许最多 10 秒的未来时钟偏差。无效或过旧的样本会阻止该节点本轮准入。
- leader 在开放 HTTP 调度接口前读取当前策略，并重建已绑定但未被指标覆盖的 Pod 预算；readiness 只在初始化完成且 HTTP 已开始监听时通过。Deployment 使用单实例 `Recreate`，升级期间会短暂停止调度；集成默认调度器时还需配置 Operator 的启动旁路，见部署文档。

80% 是调度准入的预测门槛，不是节点物理内存的绝对上限。Metrics API 有采样延迟，Pod 可能超过 request（尤其未设置 limit 时），不经过此 Extender 的其他调度路径也不受它拦截。`memory-blocked` 标签供识别和观测；实际拒绝由 Extender 的 Filter/Bind 执行。

Scheduler Extender 分数会与 kube-scheduler 的其他打分插件合并，因此这是节点偏好，不保证固定选择某个节点。可选的 `/filter_onlyone` 接口会选出最低负载对应的最高分节点；并列时按轮询分配。

集群需要正常运行 Metrics Server（`kubectl top nodes` 有返回）。Extender ServiceAccount 需要读取 Metrics API、列出 Pods，并更新 Node 标签；集成部署清单包含这些权限。

## 配置、构建和部署

`SchedulerPolicy` CRD 是集群调度策略的期望状态，API 定义和 CRD 分别位于 [api/v1alpha1](api/v1alpha1) 和 [config/crd/bases](config/crd/bases)。集群级策略对象固定命名为 `cluster`：

```bash
make install
kubectl wait --for=condition=Established --timeout=60s crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
```

策略示例位于 [config/samples/scheduler_v1alpha1_schedulerpolicy.yaml](config/samples/scheduler_v1alpha1_schedulerpolicy.yaml)。创建后可通过 `kubectl get schedulerpolicy cluster -o yaml` 查看状态。控制器状态为 `Ready=True` 表示最近一次协调成功。

### 节点保护与策略更新

- `nodeSelector` 和 `ignoreNodes` 决定 NodeBalancer 的管理范围。节点退出范围后会清理本控制器的自动 cordon；该清理先于 Metrics API 读取执行。Extender 内存准入仍检查 kube-scheduler 提供的候选节点。
- `minSchedulableNodes` 统计受管范围内 Ready、未删除、未 cordon、指标新鲜有效且没有内存保护标记或 `NoSchedule`/`NoExecute` taint 的节点。已有故障造成数量不足时停止新增 cordon；此下限不保证每个 Pod 都能调度，因为各 Pod 还有自己的资源需求、亲和性和容忍设置。
- 缺失或无效指标、节点 cordon/uncordon 失败会汇总到 `Ready=False` 的原因中，并触发重试；其他节点仍继续协调。后续一轮全部成功时恢复 `Ready=True`。CR 的 Ready 表示策略协调结果，HTTP 的 `/readyz` 表示调度服务已可接收请求。
- `scheduler.example.com/auto-cordoned=true` 表示 cordon 由控制器管理。没有该标记的人工 cordon 会保留；人工 uncordon 后留下的旧标记会清理。管理员要接管已自动 cordon 的节点时，先移除此标记，再执行自己的 cordon，见 [策略配置说明](deploy/NODEBALANCER-CONFIG.md)。

进程启动配置位于 [config/manager/scheduler.yaml](config/manager/scheduler.yaml)，只包含 metrics、健康检查、Extender 监听地址和 leader election 设置。Kubebuilder Kustomize 部署会从该文件生成 ConfigMap。

```bash
make build-image IMG=registry.example.com/platform/i-scheduler-extender:v1
make deploy IMG=registry.example.com/platform/i-scheduler-extender:v1
```

`make deploy` 会安装 CRD、manager 和 Extender Service；首次部署后仍需应用上面的 `SchedulerPolicy` 示例对象。

默认 kube-scheduler 已接入此 Extender 时，先根据实际节点修改 [bootstrap patch](config/bootstrap/manager_bootstrap_patch.yaml)，再使用 `make deploy-bootstrap IMG=...` 部署和升级。此入口通过 `nodeName` 保证 Operator 能在自身 HTTP 服务未启动时运行；当前服务器的示例节点为 `m1`，操作细节见 [启动旁路](deploy/DEPLOYMENT-STARTUP.md#单实例接管和启动旁路)。

`make deploy` 部署 Kubebuilder manager 和 Extender Service。使用自定义 `kube-scheduler` 的组合部署仍可用 [deploy/manifest.yaml](deploy/manifest.yaml)。部署细节见 [部署文档](deploy/DEPLOYMENT-STARTUP.md) 和 [集成部署文档](deploy/DEPLOYMENT-INTEGRATED.md)。

Kubebuilder 部署的 Extender Service 地址为 `http://i-scheduler-extender-http.i-scheduler-extender-system.svc:8080`，可填入已有 kube-scheduler 的 extender `urlPrefix`。

Eureka 可使用 Extender 的 `GET /health` 健康检查接口；服务返回 HTTP 200 和 `{"status":"UP"}`。例如：`http://i-scheduler-extender-http.i-scheduler-extender-system.svc:8080/health`。

## 验证调度

先确认指标可用：

```bash
kubectl top nodes
```

创建一个使用已配置调度器的 Pod，无需给节点添加标签：

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

查看调度节点和 Extender 日志：

```bash
kubectl get pod scheduler-extender-test -o wide
kubectl -n kube-system logs deployment/i-scheduler-extender -c i-scheduler-extender --tail=100
```
