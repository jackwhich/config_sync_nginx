# HTTP 参数指定发布目标

修订日期：2026-09-04。当前默认配置按类型启用发布能力，不再要求为每个站点填写 targets、健康检查或 Nginx 安装路径。对应服务能力为 `request_targets_v1`，协议主版本仍为 2。

## 配置与请求的分工

| 放在哪里 | 内容 |
| --- | --- |
| 服务配置 | listen_addr、data_dir、log_file、release_auth_tokens、repos、oras、targets |
| POST 顶层 | env、type、commit_id，可选 project |
| POST params | server_name、path_dest（每次发布必填，绝对路径） |
| 服务自动处理 | 节点主机名、默认发布锁、目标身份和持久状态、已有 Nginx 实例识别 |

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
- Git 类型从对应 `repos.config` 或 `repos.whitelist` 读取 URL 和凭据；未配置 `allowed_branches` 时允许仓库任意分支上的提交。
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
  "commit_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "project": "ybf",
  "params": {
    "server_name": "ybf-uat-nginx",
    "path_dest": "/data/nginx-publish"
  }
}
```

commit_id 必须为完整 40/64 位提交 ID，兼容 `commitid`。站点和目录推荐使用 params；兼容顶层 server_name/path_dest 以及 servee_name 拼写别名，重复提供不同值会报错。

Git 未传 branch 时从仓库分支获取提交，验证提交确实位于允许分支，再导出该提交下的 server_name 目录；传入 branch 时只验证该分支。不会把 env 猜成 Git 分支名。

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

前端只传完整 commit_id 时，服务读取 `<repository>:<完整 SHA>` 的 manifest，验证 revision annotation 和制品结构，计算确切 digest，再按 `<repository>@sha256:...` 拉取和验包。结果和状态保存实际 digest。也支持由 CI 直接传 artifact_digest；批量客户端仍要求 CI 提供固定摘要，以确保多个节点使用同一份制品。prod 不参与机器拉取。

前端落盘为 `<path_dest>/<server_name>/<完整 SHA>`，`latest` 与 SHA 目录同级。完整制品约束、CI 推送和认证初始化见 [ORAS 设计](frontend-oras.md)。

## 复用已有 Nginx

服务启动时查找已经运行的 Nginx master，在 Linux 上从 `/proc/<pid>/exe` 获取二进制，读取 master 启动参数中的 -c/-p/-g/-e。发布时使用同一实例进行语法检查，向识别出的 master 发送 HUP，然后确认新 worker 出现。不会安装、启动第二份 Nginx，也不会写入新的 nginx.conf。

单实例不需要 nginx 配置块。存在多个 master 时，可只提供已有实例的 `nginx.pid_file`；原有完整路径配置仍兼容。不能唯一识别实例或发现无法确定含义的启动参数时会明确报错，不选择一个随机实例。[Nginx 参数](https://nginx.org/en/docs/switches.html)及 [HUP reload 行为](https://nginx.org/en/docs/control.html)。

默认验证包括本地快照完整性、nginx -t、reload 和新 worker。业务 URL、Host、白名单预期行为无法从站点目录名可靠推断，因此业务 HTTP 探测是可选高级配置；未配置时不会宣称已验证业务响应。已有 Nginx include/root 必须指向对应 latest，服务不会改写现有路由。

需要业务探测、shared_assets 或逐站点限制时可使用 [高级示例](../configs/service.advanced.example.yaml)。具体站点配置仍要求其健康检查字段完整。

## 幂等、并发和恢复

简单调用可以省略 release_id 和 expected_state_revision：服务生成操作 UUID，在节点锁内基于当时的状态执行，响应返回操作 ID 和前后修订号。重复发送没有 release_id 的请求视为新操作；调用超时后若要确认原操作，使用已保存的 release_id 查询。

需要可重复重试时，调用方应在发送前生成并保存 release_id，每次重试保持请求不变。需要防止旧请求覆盖后续发布时，先 GET state，再传 expected_state_revision；这些高级字段继续生效。省略前端摘要的同 release_id 重放使用原结果，不重新解析移动后的 SHA tag。

restore_of 撤销批次仍要求 expected_state_revision，以确认没有其他发布插入；前端可省略 artifact_digest，由记录的旧基线确定，直接使用本机快照恢复。

首次状态查询或发布会登记新目标。关闭某个类型后，其尚未完成的事务仍阻止其他发布；必须重新启用原类型完成恢复。状态归属标记、切换意图、原子软链和恢复记录继续保留。

## 校验与迁移

```bash
./bin/nginx_updata_config -config configs/service.example.yaml -check-config
```

此命令只校验配置，不创建日志、状态或部署目录，也不连接 Git/Harbor、不探测 Nginx。启动与实际发布阶段分别检查运行实例和仓库连接。

兼容类型列表并不意味着可以直接接管旧 Agent 状态。已有 HTTP v2 状态的 node_id/data_dir/lock_file 应保持不变；从具体站点改为类型列表时，现有目标会从持久状态加载。较早的 Agent 状态和未管理目录仍需迁移。类型列表模式先通过 GET state 携带站点参数登记目标，再停服务执行 -adopt-target 离线导入；前端 ORAS 仍使用新空目标完成切换。
