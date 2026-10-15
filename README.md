# Kubernetes Scheduler Operator and Extender

项目骨架由 Kubebuilder v4 初始化，使用 controller-runtime manager 运行 `SchedulerPolicy` 自定义资源对应的 controller，并提供 HTTP Scheduler Extender 接口。controller 监听集群级 `SchedulerPolicy/cluster` 和 Node 变化，根据 CR 的 `spec` 协调节点 cordon 状态及 Extender 打分权重；Extender 读取 `metrics.k8s.io` 的节点负载动态打分，并阻止预计会超过内存门槛的 Pod 落到该节点。

## 调度行为

- Filter 使用节点实测内存、已调度 Pod 的内存预算和待调度 Pod 的内存预算估算调度后的内存占用；超过 Allocatable 的 80% 时给节点添加 `scheduler.example.com/memory-blocked=true` 并排除该节点。
- 节点预估内存占用降到 75% 或以下后，Filter 会清除该标签并重新允许调度。
- Prioritize 按节点实际 CPU、内存使用量占 Allocatable 的比例计算加权负载。
- 分数为 `10 * (1 - 加权使用率 / 100)`，四舍五入并限制在 0–10；负载越低，分数越高。
- `SchedulerPolicy/cluster` 中的 `cpuWeight` 和 `memoryWeight` 同时控制调度打分和节点负载保护。默认 CPU 权重 10%，内存权重 90%。
- Metrics API 结果缓存 5 秒。内存保护所需的指标或 Pod 列表不可用时，Filter 会停止本轮调度以免在未知容量下放行 Pod；动态评分失败时则返回中性分 0。
- Pod 必须声明正数的内存 request 或 limit；计算时使用 request 和 limit 中较大的值。已调度 Pod 的这些预算与当前实测内存取较大值，再加上待调度 Pod 的预算进行判断。

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

进程启动配置位于 [config/manager/scheduler.yaml](config/manager/scheduler.yaml)，只包含 metrics、健康检查、Extender 监听地址和 leader election 设置。Kubebuilder Kustomize 部署会从该文件生成 ConfigMap。

```bash
make build-image IMG=registry.example.com/platform/i-scheduler-extender:v1
make deploy IMG=registry.example.com/platform/i-scheduler-extender:v1
```

`make deploy` 会安装 CRD、manager 和 Extender Service；首次部署后仍需应用上面的 `SchedulerPolicy` 示例对象。

`make deploy` 部署 Kubebuilder manager 和 Extender Service。使用自定义 `kube-scheduler` 的组合部署仍可用 [deploy/manifest.yaml](deploy/manifest.yaml)。部署细节见 [部署文档](deploy/DEPLOYMENT-STARTUP.md) 和 [集成部署文档](deploy/DEPLOYMENT-INTEGRATED.md)。

Kubebuilder 部署的 Extender Service 地址为 `http://i-scheduler-extender-http.i-scheduler-extender-system.svc:8080`，可填入已有 kube-scheduler 的 extender `urlPrefix`。

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
