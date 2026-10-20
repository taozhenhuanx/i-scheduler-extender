# 2026-10-20 本机集群部署记录

调度逻辑和验证步骤见 [SCHEDULING-LOGIC.md](SCHEDULING-LOGIC.md)。本文记录本次构建制品、集群配置和部署操作。

## 当前进度

| 项目 | 状态 |
|---|---|
| 代码分支 | `feat/kubebuilder-operator` |
| 调度实现提交 | `8ba07ad084e8e4603538d414ff6a1d0652877802`，包含两轮优化 |
| 本机镜像 | 原缓存丢失后已用同一静态二进制重建 `lixd96/i-scheduler-extender:operator-round2-8ba07ad` |
| 当前镜像 ID | `sha256:f407ffb657e74d7faa791814e666e96a77d18d36ee058ef1f757d6020934c6e7`，与运行 Pod 的 imageID 一致 |
| 部署清单 | 已渲染 `/tmp/operator-deploy-8ba07ad.yaml`，使用 `config/bootstrap` |
| 集群应用 | 最新 Deployment、CRD、RBAC、Service 和策略已应用；策略 generation 为 `3`，`nodeSelector: {}` 已确认 |
| 根目录扩容 | 已完成，根 XFS 约 `149G`，使用 `42G`，可用 `108G`，使用率 `29%` |
| 部署健康验证 | 单副本已恢复，rollout 成功，Pod `1/1 Running`，HTTP `:8082/readyz` 返回 `ok`，策略 `Ready=True`；Service 链路及真实 scheduler Bind 已验证通过 |
| 节点磁盘压力 | 已恢复 `DiskPressure=False`，对应 taint 自动移除 |

镜像只存在当前服务器 Docker 缓存中，尚未推送镜像仓库。当前服务器为单节点 Docker 集群，Deployment 明确设置 `imagePullPolicy: IfNotPresent`。其他节点部署前需要分发或推送镜像。

## 集群检查与本次变更

- context：`kubernetes-admin@kubernetes`。
- 节点：`m1`，Kubernetes `v1.22.9`，Docker `20.10.24`。
- 默认 kube-scheduler 已配置 Extender Service，使用 `/filter`、`/prioritize`、`/bind`。业务 Pod 省略 `schedulerName` 即走这条路径。
- 部署前旧 Operator 镜像为 `operator-test-live-usage`，Deployment 使用 RollingUpdate，没有 `nodeName` 启动旁路。本次已应用 `Recreate` 和 `nodeName: m1`。
- 部署前旧 Operator 和业务 Pod 因 `node.kubernetes.io/disk-pressure:NoSchedule` 无法调度；Metrics Server 正常，`kubectl top nodes` 能返回指标。磁盘扩容后等待 kubelet 恢复节点状态，现已确认 `DiskPressure=False` 且对应 taint 自动移除；`nodeName` 旁路不能绕过 kubelet 的磁盘压力准入。
- 部署前 `SchedulerPolicy/cluster` 的 `nodeSelector` 是临时验证留下的 `codex-deployment-test/no-match: "true"`，没有管理任何节点。本次已应用正常策略样例，实际 generation 为 `3`，`nodeSelector: {}` 已确认。
- 策略样例仍使用 CPU/内存 `10/90`、高负载/恢复 `70/65`、最少可用节点 `4`、Top N `9`、周期 `5` 秒。单节点下不会新增自动 cordon，内存准入独立工作。

已备份部署前对象：

```text
/tmp/operator-before-20261020.yaml
/tmp/operator-policy-before-20261020.yaml
```

## 镜像构建方式

仓库 Dockerfile 的 `golang:1.22.5` 下载因 Docker Hub 超时失败。本次使用本机 Go `1.23.3` 和已有依赖缓存构建 Linux amd64 静态二进制，再使用 `scratch` 打包，包含 CA 证书，UID/GID 为 `65532:65532`。没有改变仓库正常的多阶段 Dockerfile。

实际构建命令：

```bash
cd /data/scheduler/i-scheduler-extender
GOCACHE=/tmp/operator-go-cache CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o /tmp/scheduler-operator-deploy-8ba07ad ./cmd
go version -m /tmp/scheduler-operator-deploy-8ba07ad
```

二进制构建信息包含 `vcs.revision=8ba07ad084e8e4603538d414ff6a1d0652877802`；`vcs.modified=true` 来自同时编写文档的工作区，调度代码仍对应该提交。

第一次构建的镜像短 ID 为 `7dce19db455b`。后续检查发现该镜像缓存已丢失，因此使用同一静态二进制重新打包相同 tag，当前镜像短 ID 为 `f407ffb657e7`。已确认完整镜像 ID 与运行 Pod 的 `imageID` 一致，详见部署后检查结果。

临时打包目录为 `/tmp/operator-image-8ba07ad`，Dockerfile 内容为：

```dockerfile
FROM scratch
LABEL org.opencontainers.image.revision="8ba07ad084e8e4603538d414ff6a1d0652877802"
LABEL org.opencontainers.image.title="i-scheduler-extender operator"
COPY manager /manager
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/manager"]
```

```bash
docker build --pull=false \
  -t lixd96/i-scheduler-extender:operator-round2-8ba07ad \
  /tmp/operator-image-8ba07ad
```

## 应用最新版本

本次已按 `config/bootstrap` 应用最新清单。以下命令保留用于复查和后续部署；先确认 `m1` 和本机镜像存在：

```bash
cd /data/scheduler/i-scheduler-extender
kubectl config current-context
kubectl get nodes
docker image inspect lixd96/i-scheduler-extender:operator-round2-8ba07ad \
  --format '{{.Id}}'
kubectl kustomize config/bootstrap > /tmp/operator-deploy-8ba07ad.yaml
```

旧 Deployment 的 RollingUpdate 字段需要一并清除，否则改为 Recreate 时 API Server 可能拒绝遗留的 `rollingUpdate` 字段。先改策略，再应用含启动旁路、最新镜像、CRD/RBAC/Service 的清单：

```bash
kubectl -n i-scheduler-extender-system patch deployment \
  i-scheduler-extender-controller-manager --type=merge \
  -p '{"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}'
kubectl apply --dry-run=server -f /tmp/operator-deploy-8ba07ad.yaml
kubectl apply -f /tmp/operator-deploy-8ba07ad.yaml
kubectl wait --for=condition=Established --timeout=60s \
  crd/schedulerpolicies.scheduler.example.com
kubectl apply -f config/samples/scheduler_v1alpha1_schedulerpolicy.yaml
kubectl -n i-scheduler-extender-system rollout status \
  deployment/i-scheduler-extender-controller-manager --timeout=120s
```

`config/bootstrap` 将 Operator 固定到 `m1`，避免默认 scheduler 已接入 Extender 时，新实例启动依赖自身尚未就绪的 HTTP 服务。此设置只用于 Operator；业务 Pod 不填写 `nodeName`。

本次扩容完成后，节点曾处于磁盘压力状态恢复等待期。新 Operator Pod 虽已通过 `nodeName` 固定到 `m1`，仍被 kubelet admission 标记为 `Evicted`。为停止控制器反复新建 Pod，曾临时将 Deployment 缩容至 `0`；`DiskPressure` 在 2026-10-20 12:52:36（北京时间，`2026-10-20T04:52:36Z`）恢复为 `False`，对应 taint 自动移除后，已恢复单副本，rollout 成功。此前产生的 182 条 Operator `Evicted` Pod 记录已清理，当前只保留新版本的 `1/1 Running` Pod。

## 部署后确认

```bash
kubectl -n i-scheduler-extender-system get pods -o wide
kubectl -n i-scheduler-extender-system get endpoints i-scheduler-extender-http
kubectl -n i-scheduler-extender-system get deployment \
  i-scheduler-extender-controller-manager \
  -o jsonpath='{.spec.strategy.type}{"\n"}{.spec.template.spec.nodeName}{"\n"}{.spec.template.spec.containers[0].image}{"\n"}'
kubectl -n i-scheduler-extender-system get pods \
  -l app.kubernetes.io/name=i-scheduler-extender \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[0].imageID}{"\n"}{end}'
kubectl get schedulerpolicy cluster -o yaml
kubectl -n i-scheduler-extender-system logs \
  deployment/i-scheduler-extender-controller-manager \
  -c manager --since=10m --tail=100
kubectl top nodes
kubectl get nodes -L scheduler.example.com/memory-blocked
```

实际检查结果：

| 项目 | 结果 |
|---|---|
| Deployment | replicas、readyReplicas、availableReplicas 均为 `1`；generation 和 observedGeneration 均为 `7`；rollout 成功 |
| 运行 Pod | `i-scheduler-extender-controller-manager-c5d7f74f7-hqgfg`，`1/1 Running`，重启 `0` 次 |
| Pod 节点与 IP | 节点 `m1`，IP `10.244.0.65` |
| Pod imageID | `sha256:f407ffb657e74d7faa791814e666e96a77d18d36ee058ef1f757d6020934c6e7`，与本机镜像一致 |
| HTTP Service Endpoint | `10.244.0.65:8080` |
| Manager 就绪接口 | 访问 Pod `:8082/readyz` 返回 `ok` |
| Extender 健康接口 | 通过 Pod IP 和 Service IP 访问 `:8080/health` 均返回 `{"status":"UP"}` |
| SchedulerPolicy | generation 和 observedGeneration 均为 `3`，`Ready=True`，`nodeSelector: {}` |
| Service DNS 与 scheduler 链路 | 修复 scheduler DNS 查询顺序后，真实默认 scheduler 经 Extender 成功 Bind 业务 Pod，链路验证通过 |

Manager 日志显示 leader 初始化完成。初始化后出现过一次更新策略 status 的 `resourceVersion` conflict，随后重试正常；NodeBalancer 稳定每 5 秒输出 `nodes=1`、`metrics=1`、`schedulable=1`、`high=0`，未见持续错误。

### 默认 scheduler 的 DNS 修复

本次链路检查发现 kube-scheduler Pod 原来使用 `ndots: 5`，DNS 搜索列表中还包含 `localdomain`。Extender Service 地址 `i-scheduler-extender-http.i-scheduler-extender-system.svc.cluster.local` 的点号数量低于 5，解析先尝试搜索域后缀；带 `localdomain` 的查询转发到外部 DNS `114.114.114.114` 后超时，拖慢了 scheduler 调用。直接向集群 DNS 查询完整名称的 A/AAAA 记录均在 `0ms` 返回，说明完整名称的集群 DNS 查询本身正常。

已先备份控制平面静态 Pod 清单：

```text
/data/scheduler/storage-expansion-20261020/kube-scheduler-before-dns.yaml
```

随后在当前服务器 kube-scheduler 静态 Pod 的 `spec.dnsConfig.options` 中设置 `ndots: "1"`，使此 Service 完整名称优先直接查询。静态 Pod 自动重建后 `Ready=True`。仓库 [kube-scheduler-static-pod.yaml](kube-scheduler-static-pod.yaml) 和 [部署说明](DEPLOYMENT-STARTUP.md) 已同步该设置。

宿主机 `curl` 默认使用宿主机的外部 DNS，单独在宿主机解析 Service 名称不能证明 scheduler 的集群 DNS 链路是否正常。本次结合集群 DNS 查询、Service IP 健康响应，以及下述真实 scheduler Bind 结果确认调用链路。

### 真实业务 Pod 的调度结果

原来等待调度的 `default/test-7c6d664cf4-86nbj` 已由 `default-scheduler` 成功绑定到 `m1`，`PodScheduled=True`。Operator 在 2026-10-20 14:19:27（北京时间）记录：

```text
bound pod: default/test-7c6d664cf4-86nbj to node:m1
```

该 Pod 已完成调度和 Extender Bind，但 Docker Hub 拉取 `busybox:1.36` 超时，随后显示 `ImagePullBackOff`。容器尚未进入 Running；这是镜像下载问题，过渡期内存预算仍保留，不能据此判断调度失败。

尝试从 `mirror.gcr.io` 拉取同版本 BusyBox 也超时。当前 `m1` 已确认缓存 `mirror.gcr.io/library/alpine:3`，已提供 [本机验证清单](scheduler-extender-check-local.yaml)，使用 `imagePullPolicy: Never`，方便用户自行验证而不依赖镜像下载：

```bash
kubectl apply -f deploy/scheduler-extender-check-local.yaml
kubectl get pod scheduler-extender-check -o wide
kubectl -n i-scheduler-extender-system logs \
  deployment/i-scheduler-extender-controller-manager -c manager --since=5m --tail=100
```

随后可按 [小 Pod 验证](SCHEDULING-LOGIC.md#62-一个小-pod-检查真实调度链路) 在本机继续试效果，通过 Manager 的 `bound pod` 日志确认实际 Bind。该服务器当前只有一个节点，无法比较多节点负载分配。

## 磁盘处理

根目录原先约 49GB、使用率 95%。部署前清理了之前工作产生的四份可再生 Go 编译缓存，空闲增加到约 7.3GB、使用率约 86%。随后按用户授权完成了新盘初始化和根目录在线扩容。

用户新增的 100GiB 磁盘已识别为 `/dev/sdb`，扩容前确认没有分区和文件系统签名。之前的执行曾被自动审批服务 HTTP 503 阻止，备份等待审批后中断；本次重新执行前成功备份了卷组元数据，再依次完成 `pvcreate`、`vgextend`、`lvextend` 和 `xfs_growfs`。

实际结果：

| 项目 | 扩容后结果 |
|---|---|
| 根 XFS | 约 `149G`，使用 `42G`，可用 `108G`，使用率 `29%` |
| 卷组 | `centos`，总容量 `148.99GiB` |
| 物理卷 | `/dev/sda2` 和新增 `/dev/sdb` |
| 执行前卷组备份 | `/data/scheduler/storage-expansion-20261020/centos-before-execution.vg`，已确认成功，约 `1.4K` |

扩容后根文件系统跨两块磁盘，两块磁盘均需保留，XFS 不能缩小。不要仅手工删除磁盘压力 taint，应让 kubelet 根据实际空间自行恢复。

以下为本次扩容操作记录，**扩容已完成，不要重新初始化 `/dev/sdb`**。执行前的只读确认命令为：

```bash
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,MODEL
wipefs --no-act /dev/sdb
```

确认空盘后，依次完成初始化、加入卷组和扩容，执行前备份使用的实际文件名如下：

```bash
mkdir -p /data/scheduler/storage-expansion-20261020
vgcfgbackup --file /data/scheduler/storage-expansion-20261020/centos-before-execution.vg centos
pvcreate /dev/sdb
vgextend centos /dev/sdb
lvextend -l +100%FREE /dev/centos/root
xfs_growfs /
df -hT /
pvs
vgs
lvs -o lv_name,vg_name,lv_size,devices
```

根目录已扩到约 149GiB，随后等待并确认 kubelet 的磁盘压力状态恢复。复查命令为：

```bash
kubectl get node m1 -o jsonpath='{.spec.taints}{"\n"}{range .status.conditions[?(@.type=="DiskPressure")]}{.status}{"\n"}{end}'
```

本次节点 `DiskPressure` 已在 2026-10-20 12:52:36（北京时间）恢复为 `False`，对应 taint 已自动移除。后续若再次发生磁盘压力，普通业务测试应等节点状态和 taint 恢复后再执行。
