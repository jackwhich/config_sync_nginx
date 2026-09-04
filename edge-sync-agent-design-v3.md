# Nginx HTTP 发布设计与实现（协议 2）

修订日期：2026-09-04。本文与本轮代码改造同步，沿用原文件名。部署架构为 HTTP 直连，没有发布中心、任务轮询、注册或结果回调。

代码已实现下述发布事务和客户端协议，自动化测试包含本地 Git、HTTP 资源、故障注入和并发检查。标准 Nginx 命令使用替身测试，未操作真实实例；第十六节区分已覆盖的自动测试和上线环境验收，不能把代码完成视为生产配置已验证。

## 一、目标与边界

Jenkins/命令行调用各节点的 HTTP 发布服务，显式提供环境、类型、站点、发布目录、分支及完整提交。配置/白名单从允许的 Git 仓库获取制品，前端通过 ORAS 拉取 Harbor 上固定 digest 的 dist.tar.gz，处理本机文件，调用已有 nginx -t/reload 命令并保存本地状态。

单次请求同步执行并返回最终结果。收到运行中冲突时调用方查询原记录，不创建后台任务队列。节点不可达时结果视为未知，批次停止推进。

支持三种类型：

| 类型 | 制品 | 生效方式 |
| --- | --- | --- |
| config | Nginx 配置 | 原子切换、nginx -t 通过后 nginx -s reload、文件校验与可选 HTTP 探测 |
| whitelist | Nginx 引用的白名单 | 同上，并配置实际放行/拒绝行为检查 |
| frontend_static | Harbor OCI 中的 dist.tar.gz | 原子 latest 切换、nginx -t/reload、文件校验与可选 HTTP 摘要校验 |

前端不执行构建，不自动修改 Nginx 路由。配置和白名单的 include 位置由运维预先配置，服务不会猜测主配置。

## 二、系统结构

```mermaid
flowchart LR
    CI[Jenkins / HTTP 批量客户端] -->|预检、查询、同步 apply| S[节点 HTTP 发布服务]
    S -->|配置/白名单：允许分支、固定提交| G[Git 仓库]
    S -->|前端：oras pull 固定 digest| H[Harbor OCI dist.tar.gz]
    S --> L[节点共享互斥锁]
    S --> D[不可变快照与 latest]
    S --> N[已有 nginx -t / reload / 可选 HTTP 验证]
    S --> J[目标事务 JSON]
    CI --> B[持久化逐节点批次记录]
```

旧 `internal/agent` 门面与调用已删除，实现已拆为 `internal/interfaces`、`internal/application`、`internal/domain` 和 `internal/infrastructure`，配置加载独立放在 `internal/config`。Jenkins 自身的 `agent any` 是构建执行器配置，与本系统部署通信方式无关。

## 三、身份与授权

- 环境以服务配置为准，请求不得跨环境。
- 默认 targets 只列允许的 type。server_name/path_dest 从 POST params 传入，首次使用登记到持久状态；保留可选逐站点高级配置。
- path_dest 规范化为真实绝对根目录。配置/白名单目录为 `<root>/<type>/<site>`；前端为 `<root>/<server_name>`，内部 `<完整SHA>` 与 `latest` 同级。
- `target_id = SHA256(JSON([env, deployment_dir]))`；同目录同环境得到相同身份，project 不参与身份。
- project 是可选约束/元数据。配置指定 project 时，省略会补为配置值，显式不一致拒绝。
- 节点有稳定 node_id。目标 `.publisher.json` 绑定 node_id、env、data_dir 和 lock_file，防止两套独立状态/锁接管同一目录。
- apply/state 使用 `X-Release-Token`。所有接口可配置来源 IP 白名单。
- XFF 仅在直连来源属于 trusted_proxy_cidrs 时采信，合并多条头后从右侧跳过可信代理，采用首个不可信地址。其左侧内容不作为身份依据。

Git URL 和凭据只来自服务配置。远端要求 HTTPS，无 URL 内嵌凭据，重定向关闭；Token 经子进程环境配置注入，不写仓库 URL、命令参数或持久化 git config。`file://` 仅供显式 allow_local 的测试/离线仓库。

## 四、配置契约

默认示例位于 `configs/service.example.yaml`，前端示例位于 `configs/frontend-service.example.yaml`。字符串 targets 为默认写法；未知字段和多份 YAML 文档仍拒绝。完整配置/请求边界见 [HTTP 参数设计](docs/request-targets.md)。

服务配置保留 data_dir、release_auth_tokens、日志、仓库连接信息和允许类型。listen_addr 默认 :9166，节点标识默认系统 hostname，默认锁为 data_dir/publish.lock；env 可按 Token 推导。Git 的 allowed_branches 可选；前端使用 oras.binary、oras.registry_config 和 oras.repository（可包含 {server_name}）。

服务不配置或识别 Nginx 实例，调用 PATH 中已有的 nginx -t，通过后执行 nginx -s reload。删除 nginx 路径块、PID/进程扫描、启动参数解析、worker 检查及直接 HUP。默认确认文件完整性、配置检查与 reload 命令结果；HTTP 探测保留为可选项。

可选 health_checks 带内容断言，可设置 URL、Host、TLS server name、HTTP 状态及 contains。`{commit}` 可用于 URL/contains，运行时替换。首次无 latest 时使用单独的 initial_health_checks；不配置探测时只校验文件基线和命令返回，不声称验证业务响应。

默认资源约束：

| 配置 | 默认值 |
| --- | --- |
| execution_timeout | 5 分钟 |
| step_timeout | 60 秒 |
| recovery_timeout | 90 秒，独立于原请求 |
| cleanup_timeout | 5 秒 |
| max_request_bytes | 64 KiB |
| max_concurrent_requests | 64 |
| max_archive_bytes / max_archive_files | 1 GiB / 100000 |
| min_free_bytes | 64 MiB |
| keep_releases | 5，最少 2 |
| asset_retention | 前端 shared_assets=true 时必须为正值；示例为 7 天 |

目录不能重叠或覆盖数据目录；部署目录内不能放锁文件。服务拥有发布目录，Nginx worker 只需读取。文件默认 0644，目录 0755；状态、清单和归属文件限制读取。操作系统应保证本地文件系统的原子 rename 与 fsync 语义。

## 五、HTTP 协议

接口仍是 apply、state、healthz、metrics 四个路由。

### 5.1 能力预检

`GET /healthz` 返回：

```json
{"status":"ok","release_contract":2,"node_id":"nginx-uat-01","env":"uat","publish_ready":true,"busy":false,"reason":""}
```

前端额外要求 capabilities 包含 `frontend_oras_v1`，缺失时拒绝发送 ORAS 发布请求。客户端在任何写入前检查全部目标节点。协议字段缺失或非 2、节点身份重复、环境不符、待恢复时停止。ready 表示发布执行器可接收工作，不是完整业务健康证明；busy 可能随时变化，最终以发布锁为准。

### 5.2 发布请求

`POST /api/v1/releases/apply` 字段：

| 字段 | 约束 |
| --- | --- |
| release_id | 可选 UUID；省略由服务生成。需要重试时调用方预先生成并保存 |
| expected_state_revision | 可选；填写时严格比对查询修订号。restore_of 恢复必填 |
| env / type | 本节点环境及允许的三种类型 |
| branch | 可选。Git 不传时检查仓库允许分支的提交可达性；前端不使用 |
| artifact_digest | 前端可选；省略时由完整 SHA tag 解析并固定摘要；Git 类型禁止传入 |
| commit_id | 必填完整 40/64 位十六进制提交 ID；兼容 commitid |
| params.server_name | 安全的单段站点名，禁止路径穿越与保留名称 |
| params.path_dest | 必填绝对根路径，和站点名共同确定本次目标 |

可选 source_repo（须等于 type）、project、version、operator、build_url、app。version 仅用于展示，不进入目录名。批次恢复额外填写 restore_of，见第九节。

请求最多一个 JSON 对象，拒绝未知字段和超限输入。令牌在解析 JSON 前检查，未授权输入不创建发布记录或业务指标标签。

### 5.3 幂等和并发结果

同一 node 范围内 release_id 唯一，绑定规范化请求、target_id 和配置来源的摘要；前端显式摘要请求来源为 repository@artifact_digest；省略摘要时重试指纹绑定原始请求和配置仓库，解析后的摘要另存结果/候选状态。相同 ID 不同参数返回 `409 RELEASE_ID_CONFLICT`。同参数运行中返回 409，终态重放原记录并标记 replayed，待恢复返回 503。

一个节点所有目标共享进程内互斥及 flock。锁覆盖接受记录、Git 缓存、快照准备、切换、Nginx 操作、状态提交与清理。其他进程启动恢复也必须持锁，禁止在另一发布进行中推断它已崩溃。

提供预期 revision 时在锁内再次检查；省略时基于锁内当前状态执行。A→B→A 每次切换都会生成新 revision，旧 revision 不会因 commit 相同重新有效。

### 5.4 状态查询

首次查询会按类型和请求参数登记目标，持久化到 data_dir/state。查询：`GET /api/v1/releases/state?env=...&type=...&server_name=...&path_dest=...`，path_dest 进行 URL 编码。后续使用 env+target_id；两种定位方式不能混用。project 可选。

附加 release_id 可查原操作。响应中的 current、previous、state_revision、observed_link 是当前目标状态；release 是历史操作结果，不能把历史 succeeded 当成当前仍在运行该版本。只进行软链观测的 GET 不等价于再次完成 Nginx 生效检查。

### 5.5 发布响应

| 字段 | 含义 |
| --- | --- |
| status | running / succeeded / skipped / failed / recovery_required |
| phase | 最近执行阶段 |
| activation_status | unchanged / reload_requested / restored / unknown |
| rollback_status | not_needed / succeeded / failed / unavailable |
| state_revision_before / after | 操作前后修订号 |
| error_code / error | 机器可识别错误与说明 |
| warnings | 不改变成功结果的清理告警 |
| steps | 有限阶段的结果和耗时 |

成功/跳过 200；无效输入 400/413；鉴权 401/403；竞争、幂等参数或修订号冲突 409；已接收操作执行失败 500；状态不确定或恢复失败 503。HTTP 连接超时只代表客户端没有拿到结论。

配置检查失败使用 NGINX_TEST_FAILED，reload 失败使用 NGINX_RELOAD_FAILED；恢复成功仍返回 HTTP 500，恢复失败使用 RECOVERY_FAILED 返回 HTTP 503。error 与 steps[].message 保留命令诊断。批量客户端输出具体错误、保存批次并非零退出，Jenkins 停止，后续节点不再发布。详见 [错误响应与 Jenkins](docs/request-targets.md#错误响应与-jenkins)。

## 六、节点执行事务

1. 认证、输入与目标校验，生成请求指纹。
2. 检查重复 ID；取得节点锁后重新读取权威状态、检查重复 ID 和 expected revision。
3. 持久化 running 接受记录。
4. 比对真实 latest 与状态基线，验证基线快照及实际入口。相同提交也必须执行这些检查才能 skipped。
5. Git 类型 fetch 允许分支，验证完整提交及分支可达性后 git archive。前端验证固定 OCI manifest digest、Git revision annotation 和有限 layer，再 oras pull 到随机目录、核验文件 SHA-256、安全解包 dist.tar.gz。
6. 受限解包，复制到随机 staging，验证必需文件、内容摘要、文件模式和可用磁盘，生成 .release-version 与清单。
7. 复用或创建不可变快照。前端使用 `<server_name>/<完整SHA>`，其他类型使用 releases/<SHA>；仅 shared_assets=true 时安装共享哈希资源。同一 SHA 的既有快照来源摘要不同会拒绝。
8. 再次确认 latest 未被外部修改，持久化候选、发布前链接、完整本地基线和 switch_intent。
9. 切换阶段使用独立于 HTTP 客户端的执行上下文，原子替换 latest。
10. 所有类型先执行 nginx -t，通过后执行 nginx -s reload，并校验本地快照。配置 HTTP 探测时验证对应 URL；前端配置 public_base_url 时额外校验公开文件摘要。
11. 一次原子状态写入同时提交 current、previous、新 revision、请求 succeeded 及最终结果。
12. 清理历史版本，清理失败记录 warnings。

第八步之前取消不会造成版本切换。进入关键阶段后，客户端断开不能取消生效或恢复；整体执行期限仍然有效。任何切换后失败都会进入独立期限的本地恢复。

子进程使用进程组，超时终止整组并收敛输出管道；输出截断并脱敏。Git archive 解包失败会关闭管道、终止生产进程并 Wait，避免旧版的管道阻塞。

## 七、文件安全与前端

### 7.1 快照

```text
<root>/<type>/<site>/
  .publisher.json
  .staging/stage-<随机 UUID>/
  releases/<完整 commit>/
  .manifests/<完整 commit>.json
  latest -> releases/<完整 commit>
```

上述布局仅用于配置/白名单；前端见 7.2 节。文件操作使用目录句柄约束（Go os.Root），解包和目录创建拒绝符号链接、硬链接、绝对路径、路径穿越、非普通文件和 `.git` 内容。临时目录和临时软链使用随机 ID，绝不使用 version。

清单保存提交、来源、文件 SHA-256 与大小。复用时校验完整文件集合和摘要，不能只凭目录存在判断成功。既有快照缺清单、内容漂移或无可信旧状态时明确失败/待恢复，禁止覆盖正在使用的目录。

### 7.2 前端 ORAS 制品

采用 [前端 ORAS 专项设计](docs/frontend-oras.md)，CI 经 HTTPS 代理向 Harbor 推送 dist.tar.gz，主机独立 ORAS 二进制按 digest 拉取，无 Docker daemon。前端目录固定为：

```text
<path_dest>/<server_name>/
  <完整SHA>/index.html
  <完整SHA>/assets/...
  latest -> <完整SHA>
  .publisher.json
  .staging/
  .manifests/<完整SHA>.json
```

latest 与完整 SHA 目录同级，不使用 releases/current。OCI artifact type 为 application/vnd.nginx.frontend.dist.v1，manifest revision annotation 必须等于请求完整 SHA。必须含名为 dist.tar.gz 的 application/gzip layer；可选 dist.tar.gz.sha256 和 manifest.json。机器按 repository@digest 拉取，prod 只给人看。

拉取前验证 manifest 的自身摘要、版本标注、layer 文件名、类型及声明大小；拉取后再次验证各文件摘要与大小。解包拒绝路径穿越、软硬链接、特殊文件、重复文件及超限输入，验证 gzip 校验和；包根必须有非空 index.html。不会在未完整校验时切换 latest。服务生成内部快照文件清单与 .release-version 探针。

默认 shared_assets=false，支持普通 dist 中的 JS/CSS/favicon 等文件，不强制前端资源哈希名或 Python 生成清单。静态引用检查用于发现缺失资源，但不能完整分析动态构造的 URL。切换后旧页面再次加载旧资源可能 404；仅保留旧 SHA 目录不会自动兼容旧页面。

需要旧页面兼容时可显式启用 shared_assets=true，继续要求 frontend-manifest.json、assets 哈希名、同名内容一致以及独立于 latest 的 /assets 路由。服务安装/保留共享资源，验证新制品及兼容窗口内旧资源；asset_retention 须覆盖旧页面寿命。

CI 先 build/tar，成功后 login/push。推送脚本保存实际推送 manifest 的 digest；全节点发布确认后才 `oras tag <repository>@<digest> prod`，并串行化应用批次。节点使用 pull-only Robot 的独立 registry_config，清空 CI 代理，验证 TLS，不能通过请求更换仓库/凭据。详见专项设计中的命令、权限和迁移要求。

## 八、权威状态和启动恢复

每个目标一个 `data_dir/state/<target_id>.json`，schema=2，包括目标、current/previous、revision、active_release_id、恢复标记及 release_id→请求/结果/基线/意图记录。

写入临时文件后 fsync，rename 替换，再 fsync 父目录。成功结果与目标 current/revision 在同一文件中提交，避免拆成多个独立权威文件。查询结果不从日志推断。

当前实现保留全部 release_id 与结果，不会因快照清理遗忘幂等历史。记录会增长；尚未实现历史明细归档，运维需监测数据盘容量，不能直接删历史 ID 释放空间。

启动时持节点锁检查：

- 无切换意图的中断操作：发布失败、链接不变。
- 有意图且 latest 为候选：校验本地快照，重新执行 nginx -t，通过后 reload并验证，确认后提交。
- latest 为原基线或候选不能确认：按本地基线恢复、验证，提交失败结果与新 revision。
- latest 不属于已记录候选/基线：保留现场，要求人工处理。
- 既有成功状态：比对当前链接、快照和 HTTP；不一致时阻止新发布。
- 已关闭发布类型（或删除高级站点配置）还有未完成状态：阻止其他目标继续发布。

状态持久化失败不能报普通成功。可报告 recovery_required，即使在线文件看起来已经生效；下次启动从最后耐久记录恢复。操作者修正外部漂移后重新启动，由同样的校验收敛。

## 九、恢复与回滚

单次发布自动恢复只使用本节点请求前保存的已验证快照，不 fetch Git，不依赖网络分支状态。重新切回原链接，执行 nginx -t，通过后 reload，校验旧文件和可选 HTTP 入口。恢复成功仍表示本次发布失败，结果为 failed + restored + rollback succeeded，并生成新 revision。

首次发布原来没有 latest，恢复删除新链接并执行 nginx -t，通过后执行 nginx -s reload；高级配置提供 initial_health_checks 时也检查原入口。恢复失败则 recovery_required，禁止后续发布。

正常人工发布旧提交可发新的 apply。撤销指定批次操作则发新 release_id，restore_of 引用同目标、同环境的一次 succeeded：

- 请求 commit 必须等于原记录的本地基线提交；前端 artifact_digest 可省略而从原基线读取；显式提供时必须等于原基线摘要。
- expected revision 必须同时等于当前 revision 与原成功操作的 after revision。
- 当前版本必须仍是原操作候选。
- 基线必须存在且完整；恢复直接使用本地快照。

原请求 skipped 不进入批次恢复。首次部署没有旧快照时批次自动恢复不可用，应在批次开始前采用 stop 策略。节点后来已被其他发布改变时返回冲突，禁止覆盖后续工作。

## 十、批量客户端与 Jenkins

节点 HTTP 服务全由 Go 实现，运行时不依赖 Python，也不会启动 Python 子进程。HTTP 协议与调用语言无关。

当前 `scripts/release_http.py` 是可选批量客户端，使用 Python 标准库；shell 文件是入口。保留 Python 的原因是该工具实现了逐节点批次落盘、未知结果查询和安全恢复，而不是服务端还存在 Agent。`scripts/frontend-manifest.py` 只用于可选 shared_assets 模式；普通 ORAS dist 使用 shell 打包推送，不需要这个 Python 工具。只需直接 HTTP 调用时，可以使用 curl、Jenkins 或其他语言；采用自建客户端仍需遵守以下持久化与恢复约束。

客户端配置 RELEASE_URLS、ENV、TYPE、COMMIT、PATH_DEST、SERVER_NAME、可选 PROJECT 及环境 Token；Git 类型提供 BRANCH，前端提供 RELEASE_ARTIFACT_DIGEST。新前端预检还要求 frontend_oras_v1 能力。

新批次先预检全部节点、确认 node_id 不重复，再把每个节点 target_id、baseline（前端含原 artifact_digest）、revision、原 UUID 和完整请求原子写到批次文件。持久化成功后才发送 HTTP。

逐节点执行，首个失败停止。默认 stop；可在开始前选择 restore，要求所有节点有旧基线。发生未知结果先按原 ID 查询，确认前不对任何节点启动回滚。重试使用原 ID/原参数。

update 只接受新批次文件，resume 从原记录继续，rollback 从同一记录逆序恢复各节点自己的基线。节点身份、当前 revision 和服务能力会再次校验。批次文件有独立进程锁，并原子更新；不保存 Token。

Jenkins 禁止同 Job 并发，归档批次文件。resume/rollback 通过 SOURCE_BUILD 读取原构建文件，示例依赖 Copy Artifact 插件。COMMIT_ID 必须显式填写制品仓库提交，不能默认使用 Jenkinsfile 所在仓库的 GIT_COMMIT。

## 十一、可观测性

### 11.1 日志契约

服务运行日志为每行一个 JSON 对象，默认 stdout；`log_file` 可指定绝对路径。统一顶层字段：`time`（UTC RFC3339Nano）、`level`（info/warn/error）、`message`、`event`。旧 `ts/msg/fields` 字段已替换，采集规则同步调整。

HTTP 访问日志 event=http_access：`request_id`、`node_id`、`env`、`client_ip`、`peer_ip`、`method`、`path`、`status_code`、`duration_ms`、`bytes_written`。发布请求补充 `release_id`、`target_id`、`release_status`、`error_code`。响应头返回服务生成的 `X-Request-ID`。

日志记录每个进入处理器的请求，包括认证失败、IP 拒绝、未知路由、限流与 panic；2xx/3xx 为 info、4xx 为 warn、5xx/panic 为 error。异常恢复在尚未写响应时返回 500。IP 与授权使用同一可信代理链解析；没有可信客户地址时为空，不使用伪造头。请求体、Token、查询字符串和完整头集合不写日志。

业务步骤与结果日志记录 release_id、target_id、有限阶段、业务 status、rollback_status 和耗时；失败使用 error。清理失败使用 warn。启动和 HTTP 服务器内部错误也通过 JSON 日志器输出。非 HTTP 事件没有访问 IP/HTTP 状态码时不补造数值。

### 11.2 指标契约

所有自定义指标前缀为 `nginx_updata_config_`：

| 指标后缀 | 类型及维度 | 含义 |
| --- | --- | --- |
| http_requests_total | Counter，handler/code | HTTP 响应数，路由固定枚举 |
| http_request_duration_seconds | Histogram，handler | HTTP 请求耗时 |
| release_terminal_total | Counter，env/release_type/target_id/status | 已接受事务的 succeeded/skipped/failed，重放不重计 |
| release_step_duration_seconds | Histogram，env/release_type/target_id/step/status | 已执行阶段耗时 |
| release_step_failures_total | Counter，env/release_type/target_id/step | 阶段失败次数 |
| rollback_total | Counter，env/release_type/target_id/status | 本地自动恢复尝试，succeeded/failed |
| cleanup_failures_total | Counter，env/release_type/target_id | 历史快照或导出目录清理失败 |
| state_persist_failures_total | Counter，env/release_type/target_id | 权威状态写入失败 |
| publish_ready | Gauge，env/node_id | 发布执行器是否可用 |
| target_recovery_required | Gauge，env/release_type/target_id | 目标是否待恢复 |
| release_in_progress | Gauge，env/release_type/target_id | 是否正在处理发布事务 |
| release_started_timestamp_seconds | Gauge，env/release_type/target_id | 当前事务开始时间，空闲为 0 |
| last_success_timestamp_seconds | Gauge，env/release_type/target_id | 当前持久版本验证时间，无版本为 0 |

commit、release_id、访问 IP 和任意请求 project 不作 Prometheus 标签。未授权/无效请求只影响有限的 HTTP 计数。目标和阶段计数器在启动时建立零值，阶段来自固定枚举（前端为 oras_pull）。恢复标记及当前版本验证时间在 health/metrics 请求时从状态视图刷新，重启后仍可观测；Counter 统计本进程事件，应使用 rate/increase 处理重启。

### 11.3 告警接入

`configs/prometheus-alerts.example.yml` 提供 9 条规则：发布失败、抓取不可达、发布不可用、步骤失败、自动恢复失败、状态写失败、清理失败、执行超时及集中 401/403。步骤标签可区分 fetch、nginx_test、reload 和文件/可选 HTTP 检查。执行超时默认 10 分钟并持续 1 分钟，应匹配部署的执行/恢复时间预算。

`configs/prometheus-scrape.example.yml` 已包含 rule_files 和 Alertmanager 连接示例；记录规则和告警文件应随配置安装。实际通知由现有 Alertmanager 接收方配置负责，本仓库不预设邮箱或 webhook。接入前用 `promtool check config`、`promtool check rules` 验证并做故障演练；本地没有 promtool 时不能把 Go 测试视为告警表达式执行验证。规则说明见 [Prometheus 官方文档](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/)。

## 十二、部署和迁移

构建要求 Go 1.25+，运行建议 Linux；Git 类型需要 Git，前端需要 ORAS 1.3.x。节点启动不会主动安装 Nginx。

支持旧式 type 列表 targets、hostname 和可选 app；不恢复 Agent 通信。简化请求需要 request_targets_v1 能力，旧服务缺少此能力时不能假设兼容。

已有文件无新状态时禁止直接发布。config/whitelist 提供 `-adopt-target/-adopt-branch/-adopt-commit` 离线导入：仅接受旧 latest 指向本目标完整提交子目录，逐文件与允许 Git 来源比对，建立快照、耐久意图后执行切换和验证。旧目录保留，中断后启动恢复。导入期间旧发布进程必须停止。

当前不自动转换旧 Git 前端目录和状态。应在新空目录验证 ORAS 制品和 Nginx latest root 后按站点流程切换入口；启用 shared_assets 才需要额外共享资源路由。旧状态文件不直接转换成新事务成功记录。

systemd 关停期限覆盖执行与恢复期限，主进程实际等待 Shutdown 和发布结束。部署示例路径应按本机安装位置调整。

## 十三、清理与异常处理

快照保留集合至少包括：current、previous、实际 latest、所有未完成请求的基线/候选、最近 keep_releases 个快照。前端还包括兼容窗口内曾使用或短暂激活的候选与基线，资源按存活清单并集保留。

清理在节点锁内执行，每个目录项检查取消；文件系统调用本身仍受操作系统 I/O 行为约束。先确认发布成功再清理，不清理 active snapshot，不改写已有内容。临时导出清理失败记录告警。

| 异常 | 处理 |
| --- | --- |
| 拉取/解包/制品校验失败 | failed，latest 不变 |
| 基线链接或内容漂移 | 阻止切换，待恢复 |
| nginx -t/reload 命令、本地文件或可选 HTTP 检查失败 | 恢复本地基线后返回 HTTP 500 和错误详情；恢复失败返回 HTTP 503 |
| 恢复失败或状态写入不确定 | recovery_required，阻止新发布 |
| 清理失败 | 已成功结果保留，warnings/日志告警 |
| 同 ID 参数变化或 revision 变化 | 409，不覆盖旧操作 |
| 批次节点超时或失联 | 结果未知，保存记录并停止推进 |

## 十四、已替换的旧行为

| 旧行为/缺陷 | 当前实现 |
| --- | --- |
| Agent 命名和门面 | HTTP 接口、应用编排、领域模型、基础设施明确分层 |
| 用 version 创建/删除工作目录 | 随机临时目录；version 仅展示 |
| latest 切换失败后不恢复 | 耐久意图、独立恢复期限和本地基线 |
| 只按 commit 跳过 | 基线链接/摘要/HTTP 验证后才 skipped |
| 无互斥与固定 latest.tmp | 节点 flock + 进程锁，随机原子软链 |
| project/site 状态键冲突 | env+真实部署目录 target_id |
| 状态直接截断写 | 临时文件 + fsync + rename + 目录 fsync |
| Git PAX 头失败和管道等待 | 接受标准元数据，错误时关闭/终止/回收生产者 |
| 从首节点 previous 统一回滚 | 每节点原成功记录与 revision 保护 |
| XFF 首段可信 | 从可信代理链右侧解析 |
| 任意请求值进入监控标签 | 配置目标与有限阶段标签 |

## 十五、代码职责

| 位置 | 职责 |
| --- | --- |
| cmd/nginx_updata_config | 配置检查、迁移入口、依赖组装、HTTP 启动和关停 |
| internal/interfaces/httpapi | 严格 JSON、认证、来源限制、HTTP 路由、访问日志 |
| internal/application/publisher | 发布事务、幂等、版本及资源策略、启动恢复与迁移 |
| internal/domain/release | 请求/响应协议、身份与参数验证 |
| internal/domain/target | 授权目标和健康检查模型 |
| internal/domain/state | 快照清单、版本、发布记录与目标事务状态 |
| internal/infrastructure/nginx | 调用已有 nginx -t、nginx -s reload 与可选 HTTP 探测 |
| internal/infrastructure/state | 事务模型的单文件 JSON 持久化 |
| internal/infrastructure/fsutil | 目录句柄、原子写与受限清理 |
| internal/infrastructure/git | Git 类型的允许来源、提交校验、归档 |
| internal/infrastructure/oras | 前端固定 digest manifest 校验、oras pull、文件核验和 gzip 解包 |
| internal/infrastructure/archive | Git/tar 共用的受限安全解包 |
| internal/infrastructure/lock / process | 共享锁、子进程期限与进程组 |
| internal/infrastructure/prom / applog | 有限指标与 JSON 日志 |
| internal/config | 严格 YAML、参数与目标映射校验 |
| scripts / Jenkinsfile | 可选客户端：持久化逐节点 HTTP 批次、前端清单工具 |

依赖规则：HTTP 适配器通过 `Publisher` 接口调用用例；应用层处理发布流程并调用基础设施，配置检查、reload 命令及可选 HTTP 探测通过 `Runtime` 接口替换。基础设施负责外部系统操作。领域模型不依赖接口层、应用层、基础设施或配置加载器。配置加载器复用领域目标类型，存储层复用领域事务类型，避免领域对象反向依赖 YAML 或 JSON 文件存储实现。

## 十六、验收

已提供自动化测试（本地 Git、HTTP 测试服务、Runtime 故障注入）：

| 场景 | 预期 |
| --- | --- |
| version 为 .. | 不删除数据目录 |
| 归档穿越/绝对路径/符号链接/硬链接 | 拒绝，部署根外不写入 |
| 预置 staging 符号链接 | 拒绝 |
| 真实 Git PAX 导出/大管道错误 | 正常导出；拒绝时及时收敛子进程 |
| 禁止分支/完整提交校验 | 拒绝非允许来源和错误对象 |
| 归档/空间/HTTP 输入限制 | 失败前不切换 |
| 同 ID 重试、重启重放、参数冲突 | 返回原结果或 409 |
| 同进程与独立 flock 竞争 | 单一节点写者 |
| 不同 data_dir/lock 接管同目录 | 归属校验拒绝 |
| A→B→A 后陈旧 revision | 409 |
| 客户端取消准备/关键阶段断开 | 准备失败不切换；关键事务继续 |
| nginx -t 失败、reload 非零退出或可选 HTTP 验证失败 | 恢复基线，HTTP 500 返回命令错误；检查失败不 reload 候选配置 |
| Jenkins 收到节点语法错误 | 输出错误码与报错详情，退出码 1，不继续发布下一台 |
| 恢复失败/成功提交落盘失败 | recovery_required，重启收敛 |
| 当前文件或 latest 漂移 | 不误报 skipped |
| 无意图中断/有意图启动恢复 | 根据耐久记录收敛 |
| 首次部署失败 | 恢复链接缺失并验证原入口 |
| restore_of 本地恢复/后续版本冲突 | 不调用 Git；不覆盖后续版本 |
| 清理失败/current/previous 保护 | 成功保留、告警、保护快照 |
| 新前端旧资源/错误 assets 路由 | 旧资源可达；错误映射触发恢复 |
| 前端缺引用或哈希路径冲突 | 切换前拒绝 |
| XFF 伪造、未认证标签输入 | 不绕过来源授权，不增加任意业务标签 |
| JSON 访问日志、401/403/404/500、panic | IP/状态码/耗时正确，令牌/查询参数/异常值不泄漏 |
| 发布失败、幂等重放、自动恢复失败指标 | 失败计数增一，重放不重计，目标待恢复 Gauge 为 1 |
| ORAS digest/revision/文件摘要不匹配、危险 layer 和 gzip | 拉取或切换前拒绝，清理半包 |
| 前端 SHA 同级目录、固定 digest、离线回滚 | 不创建 releases/current，失败不切换，不访问 Harbor 恢复 |
| 老服务预检/响应丢失/批次磁盘失败 | 写入前停止、按原 ID 查、持久化失败不 POST |
| 节点旧版本不同/恢复期间版本改变 | 各自恢复或停止 |

自动化命令测试 `go test ./internal/application/publisher -run TestStandardNginxCommandsAndRollback -v` 使用 PATH 中的替身 nginx，核对三种类型的 -t/-s reload 参数、错误详情、切换/恢复时序，以及启动和重启不扫描或控制宿主进程。

还需在目标环境执行的验收：

1. 服务账号执行已有 nginx -t 和 nginx -s reload 的权限与 PATH。
2. 真实站点 include/白名单规则、Host/SNI、文件访问权限与原有 include/root。
3. 真实前端浏览器旧页面懒加载、CDN/缓存期限、动态资源路径与跨版本路由。
4. Jenkins 凭据、持久归档和 Copy Artifact 权限；中断后使用原批次文件恢复。
5. 真实 Harbor 的 Robot、代理、CA、SHA 不可变标签及 prod 发布后更新。
6. 部署文件系统上的异常断电/fsync 行为，以及长期磁盘容量和记录归档方案。

自动化测试使用替身命令，不操作真实 Nginx。现场验证服务账号能运行既有 nginx -t 和 nginx -s reload；真实业务响应按现有运维方式验收。

## 十七、上线检查顺序

先验证服务配置、可信基线和 HTTP 入口，再按第十六节验收既有站点。升级节点服务与批量客户端，保留旧快照和原批次记录。上线逐节点检查 release_contract、node_id、目标映射和 publish_ready，首个节点确认后再推进。出现 recovery_required 时先查原 release_id 与实际链接，修复并启动恢复，不以新 ID 强行覆盖。
