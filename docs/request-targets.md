# HTTP 参数指定发布目标

修订日期：2026-09-04。当前默认配置按类型启用发布能力，不再要求为每个站点填写 targets、健康检查或 Nginx 安装路径。对应服务能力为 `request_targets_v1`，协议主版本仍为 2。

## 配置与请求的分工

| 放在哪里 | 内容 |
| --- | --- |
| 服务配置 | listen_addr、data_dir、log_file、release_auth_tokens、repos、oras、targets |
| POST 顶层 | env、type、commit_id，可选 branch、project |
| POST params | server_name、path_dest（每次发布必填，绝对路径） |
| 服务自动处理 | 节点主机名、默认发布锁、目标身份和持久状态；Nginx 命令须由发布方按顺序调用专用接口 |

```yaml
targets:
  - config
  - whitelist
  # - frontend_static
```

收到请求后先按 env 验证对应 Token，再检查 type 是否启用。params 指定这次操作的站点和目录；project 是可选元数据，不参与目标 ID。目标 ID 根据环境和真实部署目录计算，首次使用时登记到 data_dir/state，重启后重新加载和恢复。类型列表不需要随着站点数量增加而重复修改。

path_dest 须为绝对路径，server_name 须为安全的单段目录名。同一物理目录不可被两个环境、交叉嵌套目标或不同状态目录同时接管。已有未管理文件不会直接覆盖；部署入口仍应按原有迁移方式接入。

## 默认配置

完整 Git 示例见 [service.example.yaml](../configs/service.example.yaml)，前端见 [frontend-service.example.yaml](../configs/frontend-service.example.yaml)。示例中的地址和 Token 均需替换。

- `hostname` 可选，默认系统主机名；兼容 `node_id`，同时配置时必须一致。`app` 兼容为可选标识，不影响发布身份。
- `env` 可选。只配置一个环境 Token 时自动采用该环境；多个环境 Token 时按请求 env 选择，显式配置 env 可限制本节点只处理一个环境。
- `data_dir` 是固定服务状态目录。请求可额外携带同名顶层字段，但必须与配置路径一致，不能每次换一套状态/锁。
- 默认 `lock_file` 为 `<data_dir>/publish.lock`。已部署且自定义过锁路径的节点需保留原值。
- Git 类型从对应 `repos.config` 或 `repos.whitelist` 读取 URL 和凭据；本次分支由 POST 顶层 `branch` 传入，默认配置不指定分支。
- `log_file` 输出 JSON Lines；留空输出到标准输出。
- 超时、资源限制、历史版本数和来源 IP 限制保留为可选项。动态目标默认最多登记 1000 个，可用 `max_dynamic_targets` 调整。
- 明确填写 `targets` 可以只启用指定类型；省略它时根据已配置的 Git/ORAS 仓库推导类型。

## 请求示例

```http
POST /api/v1/releases/apply
Content-Type: application/json
X-Release-Token: <uat 对应的 Token>
```

```json
{
  "env": "uat",
  "type": "config",
  "branch": "uat",
  "commit_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "project": "ybf",
  "params": {
    "server_name": "ybf-uat-nginx",
    "path_dest": "/data/nginx-publish"
  }
}
```

commit_id 必须为完整 40/64 位提交 ID，兼容 `commitid`。站点和目录推荐使用 params；兼容顶层 server_name/path_dest 以及 servee_name 拼写别名，重复提供不同值会报错。

Git 发布按请求中的 `branch` 拉取对应分支，确认 `commit_id` 位于该分支的提交历史中，再导出指定提交下的 server_name 目录内容到 `<path_dest>/<type>/<server_name>/latest/`（commit 目录内不再套一层 server_name）。`env` 用于环境认证，`branch` 用于选择 Git 分支，两者独立，不会把 env 当成分支名。

`allowed_branches` 是高级配置中可选的分支允许列表，只负责限制请求能使用哪些分支，不代替请求中的 `branch`。默认示例不配置此项；若显式配置，传入的 branch 还必须在列表内。为兼容仅传 commit_id 的调用，branch 仍可省略：此时检查提交在仓库某个允许分支上可达。

## 前端 Harbor pull

```yaml
oras:
  binary: /usr/local/bin/oras
  registry_config: /etc/nginx-release/harbor-auth.json
  repository: harbor.example.com/web/{server_name}-dist
targets:
  - frontend_static
```

repository 包含 Harbor 主机地址、项目和仓库名，不带协议头、tag 或 digest。可以直接写固定仓库，也可使用唯一支持的占位符 `{server_name}`。HTTP 请求不能指定任意仓库或凭据。

前端只传完整 commit_id 时，服务读取 `<repository>:<完整 SHA>` 的 manifest，验证 revision annotation 和制品结构，计算确切 digest，再按 `<repository>@sha256:...` 拉取和验包。结果和状态保存实际 digest。也支持由 CI 直接传 artifact_digest；批量客户端省略时，由首台节点解析并返回摘要，客户端先持久化，再将它传给尚未发布的节点，保证同批次使用相同制品。prod 不参与机器拉取。

前端落盘为 `<path_dest>/<server_name>/<完整 SHA>`，`latest` 与 SHA 目录同级。完整制品约束、CI 推送和认证初始化见 [ORAS 设计](frontend-oras.md)。

## 文件同步后的检查与 reload

服务只调用机器上已经提供的命令：

```bash
nginx -t
# 仅上一步退出码为 0 时执行
nginx -s reload
```

`POST /api/v1/releases/apply` 只负责同步来源、校验候选快照并原子切换 `latest`。这是一个已完成的 Git 操作，成功后返回 HTTP 200、`status: succeeded`、阶段 `latest_switched`。它不把节点置为等待或暂停状态。

人工回滚使用独立接口，不读取 Git，也不按目录修改时间推断上一版本：

```http
POST /api/v1/releases/rollback
Content-Type: application/json
X-Release-Token: <uat 对应的 Token>

{"env":"uat","type":"config","project":"ybf","params":{"server_name":"ybf-uat-nginx","path_dest":"/data/nginx-publish"}}
```

服务从该目标的持久状态读取 `current` 和 `previous`，仅当它们是一次完整成功发布保存的相邻快照时才切换 `latest`。因此“当前 hash 的上一个版本”始终是这台机器实际保留的上一成功版本，而不是 `ls` 中时间看起来较新的目录。接口成功后返回新的 `release_id` 和 `latest_switched`；发布方必须继续对这个 ID 调用 nginx/test 与 nginx/reload。检测或 reload 失败时，服务自动恢复本次回滚前的 current。

发布方必须使用同一个 `release_id` 再依次调用以下接口。二者均使用和 apply 相同的 `X-Release-Token`：

```http
POST /api/v1/releases/nginx/test
POST /api/v1/releases/nginx/reload
Content-Type: application/json
X-Release-Token: <uat 对应的 Token>

{"env":"uat","release_id":"<apply 返回的 release_id>"}
```

- `nginx/test` 对该次 Git 切换执行 `nginx -t`。通过后返回 HTTP 200、阶段 `nginx_test_succeeded`。
- `nginx/reload` 只接受已通过检测的同一 release，执行 `nginx -s reload`、验证候选快照；成功返回 HTTP 200、阶段 `complete`。
- `POST /api/v1/releases/abort` 可由运维人员显式使用相同的 `{env, release_id}` 请求体回滚一个尚未 reload 的 Git 切换；Jenkins 不会因为自身脚本失败而自动调用它。
- 任一 Nginx 命令失败，服务立即恢复该次 Git 切换前的 `latest`，对旧配置执行 `nginx -t` 和 reload；不重新拉取 Git 或 Harbor。

`/healthz` 只反映服务进程自身是否可用，不依据发布记录、Nginx 步骤或待处理的 release 改变结果。新的 Git 同步不会自动取消、暂停或回滚上一条发布记录；已不是当前 `latest` 的旧 release 再执行 Nginx 命令会明确返回冲突，且不会改动文件。

服务配置不再接受 nginx 块。不读取 PID、不扫描进程、不解析启动参数、不检查 master/worker、不发送自行构造的 HUP，不安装、启动或停止 Nginx。已有 Nginx 主配置、include/root 由现有运维方式维护。

服务运行环境的 PATH 应能找到已有 nginx 命令，服务账号应具备配置检查和 reload 所需权限。命令缺失、非零退出或超时都会报告失败；不得只记录日志后返回发布成功。

命令成功时最终 `activation_status` 为 `reload_requested`，表示文件已切换、`nginx -t` 通过且 reload 命令成功。Nginx reload 是异步处理，默认不宣称新 worker 或业务响应已验证；有明确 URL 时，可选逐站点 HTTP 探测仍可使用。具体站点也不再要求填写 health_checks 或 public_base_url。

## 幂等、并发和恢复

简单调用可以省略 release_id 和 expected_state_revision：服务生成操作 UUID，在节点锁内基于当时的状态执行，响应返回操作 ID 和前后修订号。重复发送没有 release_id 的请求视为新操作；调用超时后若要确认原操作，使用已保存的 release_id 查询。

需要可重复重试时，调用方应在发送前生成并保存 release_id，每次重试保持请求不变。需要防止旧请求覆盖后续发布时，先 GET state，再传 expected_state_revision；这些高级字段继续生效。省略前端摘要的同 release_id 重放使用原结果，不重新解析移动后的 SHA tag。

restore_of 撤销批次仍要求 expected_state_revision，以确认没有其他发布插入；前端可省略 artifact_digest，由记录的旧基线确定，直接使用本机快照恢复。

首次状态查询或发布会登记新目标。关闭某个类型后，其尚未完成的事务仍阻止其他发布；必须重新启用原类型完成恢复。状态归属标记、切换意图、原子软链和恢复记录继续保留。

## 校验与迁移

```bash
./bin/nginx_updata_config -config configs/service.example.yaml -check-config
```

此命令只校验配置，不创建日志、状态或部署目录，也不连接 Git/Harbor、不探测 Nginx。服务启动不依赖读取 Nginx 进程；发布时由发布方在 apply 返回后依次调用 Nginx 检测和 reload 接口。

兼容类型列表并不意味着可以直接接管旧 Agent 状态。已有 HTTP v2 状态的 node_id/data_dir/lock_file 应保持不变；从具体站点改为类型列表时，现有目标会从持久状态加载。较早的 Agent 状态和未管理目录仍需迁移。类型列表模式先通过 GET state 携带站点参数登记目标，再停服务执行 -adopt-target 离线导入；前端 ORAS 仍使用新空目标完成切换。

## 错误响应与 Jenkins

`nginx -t` 失败后不 reload 候选配置。服务先恢复旧链接、检查旧配置并 reload，再由 `/api/v1/releases/nginx/test` 返回 HTTP 500；即使恢复成功，本次发布仍是失败。响应示例（省略其他事务字段）：

```json
{
  "status": "failed",
  "error_code": "NGINX_TEST_FAILED",
  "error": "nginx -t: nginx failed: exit status 1: nginx: [emerg] unknown directive in site.conf:7",
  "activation_status": "restored",
  "rollback_status": "succeeded",
  "steps": [
    {"name": "nginx_test", "status": "failed", "message": "nginx -t: nginx failed: exit status 1: nginx: [emerg] unknown directive in site.conf:7"}
  ]
}
```

reload 失败的错误码为 `NGINX_RELOAD_FAILED`。若旧配置检查或恢复 reload 也失败，返回 HTTP 503、`status: recovery_required`、`error_code: RECOVERY_FAILED`；error 同时保留原始失败和恢复失败详情，目标禁止继续发布。

`scripts/release_http.py` 将 error_code 和 error 输出到控制台，并保存到批次 JSON。它会对每台节点依次发出 apply、nginx/test、nginx/reload 请求。任一节点失败即停止后续发布，客户端退出码为 1；选择 restore 策略时只恢复已成功节点，仍以非零码结束。[Jenkinsfile](../Jenkinsfile) 只发布 config/whitelist：先显式检出配置的 GitLab 制品仓库，再用 curl 直连节点，三个动作都期待 HTTP 200。任一步未返回预期状态即打印响应体并失败，避免把 HTTP 500 当成脚本成功。

配置/白名单 Jenkins 参数与 HTTP 字段映射见 [Jenkins 发布说明](jenkins.md)。
