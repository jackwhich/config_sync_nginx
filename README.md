# Nginx HTTP 发布服务

Jenkins 或脚本通过 HTTP 同步调用每台节点。配置/白名单从允许的 Git 仓库提取，前端通过 ORAS 从 Harbor 拉取固定 digest 的 dist.tar.gz，生成不可变快照，切换 `latest`，通过 `nginx -t` 并成功执行 `nginx -s reload` 后原子提交状态。失败时用本节点发布前快照恢复。

默认配置只列 `targets: [config, whitelist]`；站点和绝对部署路径从每次 POST 的 `params` 传入，同步后执行已有的 `nginx -t`，通过后执行 `nginx -s reload`。完整约定见 [HTTP 参数与简化配置](docs/request-targets.md)。

当前代码采用 HTTP 发布协议 2。旧 `internal/agent` 入口已移除，实现按接口、应用、领域、基础设施分层。不使用拉取任务、注册、心跳上报或 desired_state。设计说明见 [HTTP 设计文档](edge-sync-agent-design-v3.md)（沿用原文件名便于追踪）。

## 代码分层

```text
cmd/nginx_updata_config/      # 程序启动、依赖组装、关停
internal/
  interfaces/httpapi/         # 路由、认证、IP 限制、JSON 访问日志
  application/publisher/      # 发布事务、幂等、恢复、清理编排
  domain/
    release/                 # 请求、结果、身份校验
    target/                  # 发布目标与健康检查模型
    state/                   # 快照、版本、事务记录模型
    auth/                    # 认证协议常量
  infrastructure/
    nginx/                   # 已有 nginx -t/reload 命令、可选 HTTP 探测
    git/                     # 配置/白名单的 Git 来源与导出
    oras/ archive/           # 前端 OCI 拉取与统一安全解包
    state/                   # 事务 JSON 持久化
    fsutil/ lock/ process/   # 文件、锁和子进程
    applog/ prom/            # JSON 日志和 Prometheus 指标
  config/                    # 配置解析与校验
scripts/                     # 可选 HTTP 客户端、制品清单生成工具
configs/                     # 服务、监控与告警配置示例
```

HTTP 层依赖 `Publisher` 接口；应用层编排领域对象和基础设施操作，`Runtime` 封装 `nginx -t`、`nginx -s reload` 和可选 HTTP 探测。领域层不依赖 HTTP、配置加载器或基础设施；磁盘存储实现与事务模型分离。配置模块负责把 YAML 转为已验证的参数。

Python 只承担两项辅助工作：`release_http.py` 持久保存多节点请求和恢复进度，`frontend-manifest.py` 仅为可选 shared_assets 模式生成哈希资源清单。普通 ORAS dist 包不需要该清单或 Python。HTTP 协议本身不绑定 Python，Go 服务不会启动这些脚本。当前保留这些可选工具，避免在客户端实现尚未替换时丢失断线续传与逐节点恢复能力。

## 构建与启动

运行依赖：Linux、已安装的 Nginx；Git 发布类型需要 Git，前端需要独立 ORAS 1.3.x；构建需要 Go 1.25+。节点服务不需要 Python。调用现有 `nginx -t` 和 `nginx -s reload`，不安装或启动 Nginx、不读 PID、不操作 master/worker，不重写现有 Nginx 主配置。可选批量客户端和前端清单工具需要 Python 3.9+，也可以由 Jenkins 或其他语言直接调用 HTTP 接口。使用 Linux 本地文件系统保存状态、锁和快照。

```bash
go test ./...
go build -o bin/nginx_updata_config ./cmd/nginx_updata_config
./bin/nginx_updata_config -config configs/service.example.yaml -check-config
./bin/nginx_updata_config -config /etc/nginx-release/service.yaml
```

`-check-config` 只校验配置结构，不创建日志或状态目录，不连接仓库或探测 Nginx。参考 [默认配置](configs/service.example.yaml)：

- `targets` 只列启用类型，`params.server_name`、`params.path_dest` 每次通过 HTTP 传入，`project` 是可选顶层字段。
- `data_dir`、环境 Token、日志和各类型的仓库连接信息留在服务配置。默认锁为 `<data_dir>/publish.lock`，已部署节点应保留原锁路径。
- 主机标识默认来自系统；支持可选 hostname/node_id。服务不读取 Nginx PID、进程列表或启动参数，不要求配置 Nginx 路径。
- 不要求配置健康检查或业务 URL；默认校验快照、执行 `nginx -t`，通过后执行 `nginx -s reload`。可选 HTTP 探测见 [高级配置](configs/service.advanced.example.yaml)。

服务配置中的令牌应限制文件读取权限。Nginx 必须具备读取快照文件和遍历部署目录的权限；默认目录 0755、文件 0644，配置可收紧。服务账号需有发布目录、状态目录写权限，并能通过 PATH 中已有的 nginx 执行 `-t` 和 `-s reload`。

可使用 [systemd unit](deploy/nginx-updata-config.service)。默认优雅退出会等待正在执行的切换和恢复；修改超时时间时同步调整 `TimeoutStopSec`。

## 仓库与目录

配置和白名单的 Git 仓库根下按站点保存制品：

```text
<repo>/
  ybf-uat-nginx/
    site.conf
```

配置和白名单落盘布局：

```text
<path_dest>/<type>/<server_name>/
  .publisher.json
  .staging/stage-<随机 UUID>/
  .manifests/<完整 commit>.json
  <完整 commit>/
    <server_name>/
      site.conf
    .release-version
  latest -> <完整 commit>
```

Nginx 原有主配置需显式 include 对应目标，例如在正确的 `http` 或 `server` 上下文中引用 `/data/nginx-publish/config/ybf-uat-nginx/latest/ybf-uat-nginx/*.conf`；仓库制品的语法上下文必须与 include 位置一致。`whitelist` 同样引用其 `latest/<server_name>` 内具体文件。服务不会修改 Nginx 主配置或自动猜测 include 位置。

Git 类型使用完整 40/64 位提交 ID；本次分支由 POST 顶层 `branch` 传入，服务验证提交位于该分支历史中。默认配置只填写仓库 URL 和凭据。拉取优先使用 Git partial clone 的 `blob:none` 过滤：先获取分支的提交和目录元数据，`git archive` 仅按需取回当前站点目录的文件；GitLab 或节点 Git 客户端不支持该协议时，会自动退回普通 fetch。高级可选项 `allowed_branches` 是额外的分支允许列表，不代替请求 branch；未传 branch 时检查提交在仓库允许分支上可达。前端使用完整 Git SHA，可由服务解析为固定 OCI digest，路径见后面的前端章节。`version` 仅用于展示。归档拒绝符号链接、硬链接、绝对路径和穿越路径，设有大小、文件数、执行时间限制。相同提交的快照经过清单校验后复用，不能原地改写。

发布流程为：拉取 → 校验文件 → 建立完整 SHA 快照 → 原子切换 latest → `nginx -t` → `nginx -s reload` → 校验本地快照（及可选 HTTP 探测）→ 提交状态。检查或 reload 命令失败时恢复旧链接，再执行 `nginx -t`，通过后 reload；恢复失败则阻止后续发布。默认成功结果的 `activation_status` 为 `reload_requested`，表示文件就位且命令成功，不代表检查过 worker 或业务响应。

`nginx -t` 失败返回 HTTP 500、`error_code: NGINX_TEST_FAILED` 和命令输出；reload 失败返回 `NGINX_RELOAD_FAILED`。恢复失败返回 HTTP 503、`RECOVERY_FAILED`，保留原始错误和恢复错误。批量客户端输出这些详情，以退出码 1 结束，Jenkins 的 `sh` 步骤随即失败，后续节点不继续发布。详见 [错误响应与 Jenkins](docs/request-targets.md#错误响应与-jenkins)。

## HTTP 接口

四个接口保持不变：

| 方法及路径 | 用途 |
| --- | --- |
| `GET /healthz` | 协议能力、node_id、publish_ready、busy |
| `GET /api/v1/releases/state` | 目标当前状态，或按 release_id 查询历史结果 |
| `POST /api/v1/releases/apply` | 同步发布，或按原成功记录恢复 |
| `GET /metrics` | 有限维度的 Prometheus 指标 |

apply/state 使用 `X-Release-Token`。所有接口受配置的来源 IP 约束。受信反代的 XFF 从右侧逐跳解析，忽略首个不可信节点左侧的伪造地址。

简单发布请求如下，`commitid` 也可作为 `commit_id` 的兼容别名：

```json
{"env":"uat","type":"config","commit_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project":"ybf","params":{"server_name":"ybf-uat-nginx","path_dest":"/data/nginx-publish"}}
```

省略 release_id 时服务生成 UUID，响应返回该 ID；需要可重试操作时，应由调用方预先生成并保存 release_id。省略 expected_state_revision 时在节点锁内使用当前状态；需要防止过时请求覆盖后续发布时，使用下面的完整流程。简化请求要求 healthz capabilities 包含 `request_targets_v1`。

先检查 `/healthz`：`release_contract` 必须等于 2，`publish_ready` 必须为 true。前端还要求 `capabilities` 包含 `frontend_oras_v1`。老服务缺少该字段时客户端停止。再查询目标并保存 `target_id`、`state_revision`：

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
export RELEASE_URLS='http://10.0.0.11:9166,http://10.0.0.12:9166'
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

保存批次文件到持久存储，文件内没有令牌。[Jenkinsfile](Jenkinsfile) 只用于 config/whitelist，显式检出其中配置的 GitLab 制品仓库，再以该仓库的 HEAD 发布；构建参数只暴露 ACTION、RELEASE_TYPE 和 SERVER_NAME。它用 curl 直连节点 HTTP，不调用批量脚本。GitLab 检出凭据与节点 Token 分别配置；前端发布和回滚使用独立 Jenkinsfile。详见 [Jenkins 发布说明](docs/jenkins.md)。Jenkinsfile 中 `agent any` 是 Jenkins 执行器语法，部署方式仍为 HTTP。

## 前端 ORAS 制品发布

前端使用 Harbor OCI 文件制品，主机无需 Docker。完整方案、命令和回滚约束见 [ORAS 前端设计](docs/frontend-oras.md)。目录按本次约定：

```text
<path_dest>/<server_name>/
  <完整SHA>/index.html
  <完整SHA>/assets/...
  latest -> <完整SHA>
```

例如 `/var/www/app/<完整SHA>`，Nginx root 为 `/var/www/app/latest`。latest 与 SHA 目录同级，不使用 releases/current。

- CI 先 npm build，再用 `bash scripts/frontend-artifact.sh push dist ../artifact-bundle` 打包并经 HTTPS_PROXY 推送 SHA tag。脚本生成 `artifact.digest`，不更新 prod。
- 前端 Harbor 地址从 `oras.repository` 读取，可包含 `{server_name}`；支持完整固定仓库名。未传 artifact_digest 时从完整 SHA tag 解析 manifest 后按摘要 pull；传入摘要时直接使用。前端不访问 Git。
- 服务按 `repository@digest` 拉取，验证 OCI revision、tar 文件摘要和安全解包后，原子 rename 替换 latest。调用已有 `nginx -t`，通过后执行 `nginx -s reload` 并校验本地文件；HTTP 探测可选，失败仅用本地基线恢复。
- 所有节点确认成功后，CI 用 `oras tag "$HARBOR_REPOSITORY@$RELEASE_ARTIFACT_DIGEST" prod` 更新展示标签。

配置见 [前端服务示例](configs/frontend-service.example.yaml) 和 [Nginx root 示例](configs/frontend-location.example.conf)。云主机 ORAS 子进程清空代理；pull-only Robot 凭据保存在独立 registry_config 文件，使用 --password-stdin 登录。

普通 dist 模式 `shared_assets: false` 只要求根目录有非空 index.html，允许 app.js、favicon.ico 等普通资源，不再要求 Python 清单工具。服务仍检查发现的本地静态引用。旧页面懒加载兼容需另外启用 `shared_assets: true`、共享 /assets 路由和哈希资源清单；旧 SHA 目录留在磁盘不会自动保证旧 URL 可达。

批量客户端支持 update/resume/rollback。前端 `RELEASE_ARTIFACT_DIGEST` 可选；省略时首台节点按完整 SHA 解析摘要，客户端先持久化该摘要，再用于后续节点，保证同批次使用相同制品。恢复时从每节点原始基线读取自己的 digest，不重新拉取 Harbor。

## JSON 日志与告警

所有服务运行日志逐行输出 JSON，默认 stdout，可用 `log_file` 指定绝对文件路径。基础字段统一为 `time`（UTC RFC3339）、`level`、`message`、`event`；不再使用旧的 `ts/msg/fields` 结构，日志采集规则需要同步更新。

一次 HTTP 请求完成后的访问日志示例：

```json
{"time":"2026-09-04T08:30:00Z","level":"info","message":"HTTP 请求完成","event":"http_access","request_id":"70a0aa0b-ed68-4abf-9f76-58fe71777dfe","client_ip":"192.0.2.10","peer_ip":"10.0.0.2","method":"POST","path":"/api/v1/releases/apply","status_code":200,"duration_ms":1520.5,"bytes_written":860,"node_id":"nginx-uat-01","env":"uat"}
```

访问日志覆盖进入处理器的成功、401/403、404/405、限流和 500 请求。2xx/3xx 为 info，4xx 为 warn，5xx/异常为 error。`client_ip` 使用与访问控制相同的可信代理解析，`peer_ip` 为实际连接来源；响应头 `X-Request-ID` 可关联日志。发布请求还记录 `release_id/target_id/release_status/error_code`。不记录 Token、请求体、查询字符串或全部请求头。

发布步骤和最终结果日志包含 `release_id`、`target_id`、`status`、`duration_ms`，失败使用 error 级别。服务内部及启动失败日志也使用 JSON。非 HTTP 事件不伪造访问 IP 或 HTTP 状态码。

`GET /metrics` 输出 Prometheus 指标。完整清单见 [设计文档的可观测性章节](edge-sync-agent-design-v3.md#十一可观测性)。已配置目标的计数器启动时初始化为 0，幂等重放不会增加发布结果计数；`commit/release_id/client_ip` 不作为指标标签。

| 告警 | 默认触发条件 |
| --- | --- |
| NginxHTTPReleaseFailed | 10 分钟内发布失败 |
| NginxHTTPReleaseUnavailable | Prometheus 连续 2 分钟抓取失败 |
| NginxHTTPReleaseNeedsRecovery | 发布不可用持续 1 分钟 |
| NginxHTTPReleaseStepFailed | 10 分钟内任一发布步骤失败，可按 step 定位 Git、oras_pull、nginx_test、reload 等 |
| NginxHTTPReleaseRollbackFailed | 10 分钟内本地自动恢复失败 |
| NginxHTTPReleaseStateWriteFailed | 10 分钟内权威状态写入失败 |
| NginxHTTPReleaseCleanupFailed | 10 分钟内快照或导出目录清理失败 |
| NginxHTTPReleaseStuck | 正在执行超过 10 分钟，持续 1 分钟；需按 execution/recovery 超时调整 |
| NginxHTTPReleaseAccessDenied | 5 分钟内同一实例 401/403 至少 10 次 |

接入步骤：

1. 将 [抓取配置](configs/prometheus-scrape.example.yml)、[记录规则](configs/prometheus-recording.example.yml)、[告警规则](configs/prometheus-alerts.example.yml) 放在同一 Prometheus 配置目录，合并配置或使用该抓取配置启动 Prometheus。
2. 修改节点地址、Alertmanager 地址，将 Prometheus 来源加入服务 `allowed_client_ips`。
3. 执行 `promtool check config configs/prometheus-scrape.example.yml` 和 `promtool check rules configs/prometheus-alerts.example.yml configs/prometheus-recording.example.yml`，然后重载 Prometheus。
4. 在已有 Alertmanager 配置通知接收方，并做一次故障演练。仓库提供指标和规则，不代表已经接入实际通知渠道。

规则编写和通知机制参考 [Prometheus 告警规则文档](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/)。本地未安装 promtool 时，应在监控部署环境执行上述检查。

## 旧版本迁移

支持字符串 targets、hostname 和可选 app。旧 Agent 状态按 project/site 定位，仍不能直接当作 HTTP v2 状态；已有 HTTP v2 状态可在保持 node_id/data_dir/lock_file 不变时从具体站点配置切换为类型列表。

1. 停止旧发布进程，保存旧配置、状态和所有生效快照。
2. 根据新示例配置显式目标、节点身份、共享锁和生效检查，先运行 `-check-config`。
3. 空部署目录可以初始化。已有文件或旧 latest 而没有可信新状态的目标显示 `publish_ready=false`，不会被覆盖。
4. config/whitelist 可离线导入 `latest -> <完整 commit>` 的旧快照（相对链接，或指向本目标该子目录的绝对链接）：

```bash
nginx_updata_config -config /etc/nginx-release/service.yaml \
  -adopt-target '<health/state 返回的 target_id>' \
  -adopt-branch uat -adopt-commit '<旧快照完整 commit>'
```

导入逐文件比对允许仓库的指定提交，建立新清单和本地基线后持久化切换意图，执行切换、`nginx -t`、`nginx -s reload` 和可选 HTTP 验证。保留旧目录。中断后由新服务启动恢复。文件不匹配、存在未完成发布或链接不属于本目标时拒绝导入。

旧 Git 前端目录和状态不自动转换为 ORAS 布局；先在新空 `<path_dest>/<server_name>` 目录验收，再明确切换 Nginx root。请在新空目标完成制品发布与路由验证，再按站点变更流程切换入口。`node_id/data_dir/lock_file` 归属已建立后不应随意更换或手工删除标记。

本轮改造前代码和旧发布包备份在 `/private/tmp/nginx-before-http-vzttmqfe`，临时目录不适合长期备份，请按团队要求归档。

## 验证与发布包

```bash
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts -p '*_test.py'
# 标准命令与恢复流程测试：使用临时 nginx 替身，不启动真实 Nginx
go test ./internal/application/publisher -run TestStandardNginxCommandsAndRollback -v
make dist-linux-amd64 VERSION=http-v2
make dist-linux-arm64 VERSION=http-v2
```

自动化测试覆盖真实本地 Git 导出、恶意归档、HTTP 前端资源、并发与幂等、发布失败、状态写入失败、启动恢复、批次超时与逐节点恢复等。标准命令测试覆盖三种发布类型的 `-t`/reload 顺序、语法错误详情、失败恢复和幂等重放；HTTP 测试与客户端子进程测试验证错误响应和退出码 1，失败后不发布下一节点。测试使用替身 Nginx；实际 include、权限和业务响应需在目标环境验收。
