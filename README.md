# Nginx HTTP 发布服务

Jenkins 或脚本通过 HTTP 同步调用每台节点。服务从配置允许的 Git 仓库提取指定站点和完整提交，生成不可变快照，切换 `latest`，验证 Nginx 生效后原子提交状态。失败时用本节点发布前快照恢复。

当前代码采用 HTTP 发布协议 2。旧 `internal/agent` 入口已移除，实现位于 `internal/service`。不使用拉取任务、注册、心跳上报或 desired_state。设计说明见 [HTTP 设计文档](edge-sync-agent-design-v3.md)（沿用原文件名便于追踪）。

## 构建与启动

运行依赖：Linux、Git、已安装的 Nginx；构建需要 Go 1.25+。客户端需要 Python 3.9+。使用 Linux 本地文件系统保存状态、锁和快照。

```bash
go test ./...
go build -o bin/nginx_updata_config ./cmd/nginx_updata_config
./bin/nginx_updata_config -config configs/service.example.yaml -check-config
./bin/nginx_updata_config -config /etc/nginx-release/service.yaml
```

`-check-config` 校验配置结构与目录映射，不检查仓库连通性、Nginx 安装或业务路由。参考 [service.example.yaml](configs/service.example.yaml)，至少配置：

- 稳定的 `node_id`、环境、发布令牌及来源地址。
- 本节点共享的 `data_dir` 和 `lock_file`。同一目标不能被不同状态目录或锁接管，`.publisher.json` 保存其归属。
- 明确的 `type + server_name + path_dest` 目标、允许仓库和分支；`project` 可选，不参与目标身份。
- Nginx 的绝对二进制、主配置、prefix、PID 文件路径。四者必须对应同一个实例。
- 实际生效检查 `health_checks`。检查地址应直接命中本节点，可指定 Host 和 TLS server name；不能经过负载均衡随机访问其他节点。
- 首次无 `latest` 时的 `initial_health_checks`，例如原入口返回 404。恢复首次失败时也用这些检查验证链接缺失的原状态。

服务配置中的令牌应限制文件读取权限。Nginx 必须具备读取快照文件和遍历部署目录的权限；默认目录 0755、文件 0644，配置可收紧。服务账号需有发布目录、状态目录及指定 Nginx 实例的操作权限。

可使用 [systemd unit](deploy/nginx-updata-config.service)。默认优雅退出会等待正在执行的切换和恢复；修改超时时间时同步调整 `TimeoutStopSec`。

## 仓库与目录

仓库根下按站点保存制品：

```text
<repo>/
  ybf-uat-nginx/
    site.conf
```

落盘布局：

```text
<path_dest>/<type>/<server_name>/
  .publisher.json
  .staging/stage-<随机 UUID>/
  .manifests/<完整 commit>.json
  releases/<完整 commit>/
    site.conf
    .release-version
  assets/                       # 仅前端类型，跨版本共享
  latest -> releases/<完整 commit>
```

Nginx 原有主配置需显式 include 对应目标，例如在正确的 `http` 或 `server` 上下文中引用 `/data/nginx-publish/config/ybf-uat-nginx/latest/*.conf`；仓库制品的语法上下文必须与 include 位置一致。`whitelist` 同样引用其 `latest` 内具体文件。服务不会修改 Nginx 主配置或自动猜测 include 位置。

提交必须是允许分支可达的完整 40/64 位 ID。`version` 仅用于展示。归档拒绝符号链接、硬链接、绝对路径和穿越路径，设有大小、文件数、执行时间限制。相同提交的快照经过清单校验后复用，不能原地改写。

## HTTP 接口

四个接口保持不变：

| 方法及路径 | 用途 |
| --- | --- |
| `GET /healthz` | 协议能力、node_id、publish_ready、busy |
| `GET /api/v1/releases/state` | 目标当前状态，或按 release_id 查询历史结果 |
| `POST /api/v1/releases/apply` | 同步发布，或按原成功记录恢复 |
| `GET /metrics` | 有限维度的 Prometheus 指标 |

apply/state 使用 `X-Release-Token`。所有接口受配置的来源 IP 约束。受信反代的 XFF 从右侧逐跳解析，忽略首个不可信节点左侧的伪造地址。

先检查 `/healthz`：`release_contract` 必须等于 2，`publish_ready` 必须为 true。老服务缺少该字段时客户端停止。再查询目标并保存 `target_id`、`state_revision`：

```bash
curl --get "$RELEASE_URL/api/v1/releases/state" \
  -H "X-Release-Token: $RELEASE_TOKEN" \
  --data-urlencode 'env=uat' \
  --data-urlencode 'type=config' \
  --data-urlencode 'server_name=ybf-uat-nginx' \
  --data-urlencode 'path_dest=/data/nginx-publish'
```

用查询到的 revision 和本次新生成的 UUID 发出请求：

```json
{
  "release_id": "70a0aa0b-ed68-4abf-9f76-58fe71777dfe",
  "expected_state_revision": "e2f5a517-dc90-4258-ae0a-9ab4eb319626",
  "env": "uat",
  "type": "config",
  "branch": "uat",
  "commit_id": "0123456789abcdef0123456789abcdef01234567",
  "project": "ybf",
  "params": {
    "server_name": "ybf-uat-nginx",
    "path_dest": "/data/nginx-publish"
  }
}
```

示例中的 ID 和 commit 为占位值。请求体必须只有一个 JSON 对象，拒绝未知字段和超限请求。`source_repo` 可省略；填写时必须与 type 相同。

同一 release_id、相同规范化参数会重放记录；同一 ID 参数变化返回 409。运行中返回 409，待恢复返回 503。状态修订号冲突也返回 409，防止覆盖后续发布，包括 A→B→A 的情况。

响应保留逐步耗时、`status`、`activation_status`、`rollback_status`、前后修订号及错误码。成功或跳过为 200，已接收发布执行失败为 500，状态不确定或恢复失败为 503。HTTP 超时不是失败结论，查询：

```text
GET /api/v1/releases/state?env=uat&target_id=<目标 ID>&release_id=<原请求 UUID>
```

返回的 `release` 是历史结果；`state_revision`、`current` 和 `observed_link` 是当前观测，不能把历史成功当成目前仍生效。

## 多节点调用与恢复

优先使用脚本，它会在发布前检查全部节点、保存逐节点基线和 UUID，并将每次结果原子写入批次文件：

```bash
export RELEASE_URLS='http://10.0.0.11:8081,http://10.0.0.12:8081'
export RELEASE_ENV=uat RELEASE_TYPE=config RELEASE_BRANCH=uat
export RELEASE_PROJECT=ybf RELEASE_SERVER_NAME=ybf-uat-nginx
export RELEASE_PATH_DEST=/data/nginx-publish
export RELEASE_COMMIT='<制品仓库的完整提交 ID>'
# RELEASE_TOKEN 从 CI 凭据或当前终端环境提供
bash scripts/release-apply.sh update --batch-file release-batch-001.json
bash scripts/release-apply.sh resume --batch-file release-batch-001.json
bash scripts/release-apply.sh rollback --batch-file release-batch-001.json
```

`update` 要求新批次文件；`resume/rollback` 必须用原文件，不能重新生成请求 ID。恢复按每个节点原记录中的基线执行，不再读取第一台节点的 previous 并统一发布。原操作 skipped 的节点不会恢复；节点后续已被修改时停止并报告修订号冲突。

默认失败策略为 `stop`，可在新批次开始前指定 `--failure-policy restore`。后者要求每台节点已有可恢复快照；首次部署使用 stop 并事先确定人工退出策略。未知结果未确认之前不启动任何恢复。恢复失败或基线变化会停止处理，保留记录供排查。

保存批次文件到持久存储，文件内没有令牌。Jenkins 示例会归档该文件，resume/rollback 用 `SOURCE_BUILD` 读取原构建记录，需要 **Copy Artifact** 插件。Jenkinsfile 中 `agent any` 是 Jenkins 执行器语法，部署方式仍为 HTTP。

## 前端静态资源

前端仓库需包含 `index.html`、`assets/` 中带十六进制哈希名的资源和 `frontend-manifest.json`。可在提交制品前生成清单：

```bash
python3 scripts/frontend-manifest.py path/to/ybf-uat-web
```

资源名示例 `assets/app.89abcdef.js`；清单存储文件 SHA-256。除 index 和清单外，所有制品必须列为哈希资源。当前静态引用检查覆盖 HTML src/href、CSS url、常见 JS import/URL 写法；运行期拼接的 URL 需要构建规范保证，不能靠源码正则证明完整性。

采用 [前端 Nginx 路由示例](configs/frontend-location.example.conf)，让 `/assets/` 指向站点的共享 assets 目录，让 HTML 和版本探针不缓存。配置 `public_base_url` 和必要的 `public_host`，服务将验证新版本全部制品的 HTTP 摘要及保留版本的旧资源可达性。

`asset_retention` 必须覆盖客户端存活、缓存和懒加载窗口。清理保护当前、上一版本、运行中候选/基线及前端兼容窗口，至少保留 `keep_releases` 个最近创建快照。清理失败只返回 warnings；不撤销已确认的成功。

## 旧版本迁移

旧配置不能直接用于新服务；严格解析会拒绝字符串 targets、hostname、app 等旧字段。旧状态按 project/site 定位，不能直接当作新目标状态。

1. 停止旧发布进程，保存旧配置、状态和所有生效快照。
2. 根据新示例配置显式目标、节点身份、共享锁和生效检查，先运行 `-check-config`。
3. 空部署目录可以初始化。已有文件或旧 latest 而没有可信新状态的目标显示 `publish_ready=false`，不会被覆盖。
4. config/whitelist 可离线导入 `latest -> <完整 commit>` 的旧快照（相对链接，或指向本目标该子目录的绝对链接）：

```bash
nginx_updata_config -config /etc/nginx-release/service.yaml \
  -adopt-target '<health/state 返回的 target_id>' \
  -adopt-branch uat -adopt-commit '<旧快照完整 commit>'
```

导入逐文件比对允许仓库的指定提交，建立新清单和本地基线后持久化切换意图，执行切换、Nginx 校验、reload 和 HTTP 验证。保留旧目录。中断后由新服务启动恢复。文件不匹配、存在未完成发布或链接不属于本目标时拒绝导入。

前端旧布局需先适配共享资源路由；当前导入命令不自动转换旧前端。请在新空目标完成制品发布与路由验证，再按站点变更流程切换入口。`node_id/data_dir/lock_file` 归属已建立后不应随意更换或手工删除标记。

本轮改造前代码和旧发布包备份在 `/private/tmp/nginx-before-http-vzttmqfe`，临时目录不适合长期备份，请按团队要求归档。

## 验证与发布包

```bash
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts -p '*_test.py'
# 可选：实际安装的 Nginx，测试自行创建专用实例/端口/目录
NGINX_TEST_BINARY=/usr/sbin/nginx go test ./internal/service/runner -run TestRealNginxActivationAndRecovery -v
make dist-linux-amd64 VERSION=http-v2
make dist-linux-arm64 VERSION=http-v2
```

自动化测试覆盖真实本地 Git 导出、恶意归档、HTTP 前端资源、并发与幂等、发布失败、状态写入失败、启动恢复、批次超时与逐节点恢复等。当前机器未安装 Nginx，下载未获批准，真实 Nginx 专项测试未执行；默认测试会显式跳过该项。上线前须在目标 Nginx 环境执行该测试，并验证实际 include、Host、权限、缓存与探针配置。
