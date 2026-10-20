# 2026-10-20 本机集群部署记录

调度逻辑和验证步骤见 [SCHEDULING-LOGIC.md](SCHEDULING-LOGIC.md)。本文记录本次构建制品、集群配置和部署操作。

## 当前进度

| 项目 | 状态 |
|---|---|
| 代码分支 | `feat/kubebuilder-operator` |
| 调度实现提交 | `8ba07ad084e8e4603538d414ff6a1d0652877802`，包含两轮优化 |
| 本机镜像 | 已构建 `lixd96/i-scheduler-extender:operator-round2-8ba07ad` |
| 镜像 ID | `7dce19db455b`，完整 ID 可用 `docker image inspect` 查询 |
| 部署清单 | 已渲染 `/tmp/operator-deploy-8ba07ad.yaml`，使用 `config/bootstrap` |
| 集群应用 | 尚未执行；自动审批服务连续返回 HTTP 503，服务端 dry-run 未执行 |
| 部署健康验证 | 尚未进行，不能据此认为新版本已上线 |

镜像只存在当前服务器 Docker 缓存中，尚未推送镜像仓库。当前服务器为单节点 Docker 集群，Deployment 明确设置 `imagePullPolicy: IfNotPresent`。其他节点部署前需要分发或推送镜像。

## 集群检查结果

- context：`kubernetes-admin@kubernetes`。
- 节点：`m1`，Kubernetes `v1.22.9`，Docker `20.10.24`。
- 默认 kube-scheduler 已配置 Extender Service，使用 `/filter`、`/prioritize`、`/bind`。业务 Pod 省略 `schedulerName` 即走这条路径。
- 旧 Operator 镜像为 `operator-test-live-usage`，Deployment 使用 RollingUpdate，没有 `nodeName` 启动旁路。
- 检查时旧 Operator 和业务 Pod 因 `node.kubernetes.io/disk-pressure:NoSchedule` 无法调度；Metrics Server 正常，`kubectl top nodes` 能返回指标。
- 旧 `SchedulerPolicy/cluster` 的 `nodeSelector` 是临时验证留下的 `codex-deployment-test/no-match: "true"`，没有管理任何节点。本次计划应用正常策略样例，恢复 `nodeSelector: {}`。
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

下面的命令应在当前服务器执行。先确认 `m1` 和本机镜像存在：

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

预期 Operator `1/1 Running`，HTTP Service 有 Endpoint，镜像与本机镜像 ID 一致，策略 `Ready=True` 且 `observedGeneration` 与 generation 一致，`nodeSelector` 恢复为空。

随后按 [小 Pod 验证](SCHEDULING-LOGIC.md#62-一个小-pod-检查真实调度链路) 在本机试效果，通过 Manager 的 `bound pod` 日志确认实际 Bind。该服务器当前只有一个节点，无法比较多节点负载分配。

## 磁盘处理

根目录原先约 49GB、使用率 95%。已清理之前工作产生的四份可再生 Go 编译缓存，空闲增加到约 7.3GB、使用率约 86%；Kubernetes 磁盘压力状态有恢复等待时间，应再次查询确认。

用户新增的 100GiB 磁盘已识别为 `/dev/sdb`，检查无分区和文件系统签名。用户已同意将它加入 `centos` LVM 并在线扩根 XFS；实施结果另行记录。扩容后根文件系统跨两块磁盘，两块磁盘均需保留。不要仅手工删除磁盘压力 taint，应让 kubelet 根据实际空间自行恢复。
