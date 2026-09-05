# Jenkins HTTP 发布流水线

修订日期：2026-09-05。仓库根目录的 `Jenkinsfile` 只用于 config 发布；白名单与 `frontend_static` 均使用独立 Jenkinsfile 和 Job。它显式检出 GitLab 制品仓库的 `uat` 分支，使用该工作区的 HEAD 作为 `commit_id`。流水线用 curl 直连节点 HTTP，不调用 `scripts/release-apply.sh`。

## Job 与执行环境

Job 可以使用 Pipeline from SCM 读取本仓库的 `Jenkinsfile`；流水线会跳过默认检出，再用 Git 插件显式检出制品仓库 `https://dcproopsgitlab.opscom999.com/dc-ops/nginx_vhost.git` 的 `uat` 分支。因此发布使用的是制品仓库的 HEAD，而非 Jenkinsfile 所在仓库的 SHA。`RELEASE_SOURCE_CREDENTIAL_ID=gitlab_pushom` 是该 GitLab 仓库的 Jenkins 凭据 ID；`RELEASE_CREDENTIAL_ID=release-token-uat` 则是节点 HTTP 发布 Token，两者不可混用。

环境、分支、部署目录、节点地址和凭据 ID 写在 `Jenkinsfile` 的 `environment` 中。当前 Job 固定 `RELEASE_TYPE=config`，构建页面选择 `ACTION=update` 或 `ACTION=rollback`，以及 `SERVER_NAME`。白名单发布应使用独立的 Jenkinsfile，并固定为 `RELEASE_TYPE=whitelist` 与对应的 GitLab 仓库、凭据。

执行器需要 curl，能够访问节点 HTTP 地址。流水线使用 Pipeline 和 Credentials Binding。Jenkins 的 `agent any` 是执行器声明，节点发布通信使用 HTTP。

在 Jenkins 中保存 Secret text 凭据，ID 与 `RELEASE_CREDENTIAL_ID` 一致，内容为节点 `release_auth_tokens` 中对应环境的 Token。凭据只放进 `X-Release-Token` 请求头。

## 构建参数与 HTTP 字段

| 来源 | HTTP 字段或用途 |
| --- | --- |
| ACTION=update | 检出制品仓库 HEAD 并发布该 commit |
| ACTION=rollback | 将当前 latest 回滚到该节点保留的上一成功快照；不检出、不拉取 Git |
| RELEASE_TYPE | 固定顶层 type：config |
| SERVER_NAME | params.server_name |
| `nginx_vhost.git` 的 `uat` 分支 HEAD | 顶层 commit_id |
| `RELEASE_BRANCH`（默认 uat） | 顶层 branch |
| environment 固定值 | env、project、path_dest、节点 URL、GitLab 地址及凭据 ID |

Jenkins 界面中发布拆为三个独立阶段：`同步 Git 并切换 latest`、`nginx -t 配置检测`、`nginx -s reload 与生效验证`。每个节点先 `GET /healthz`，确认服务状态为 `ok` 且 `release_contract` 为 2；`update` 的第一阶段调用 `POST /api/v1/releases/apply`，`rollback` 调用 `POST /api/v1/releases/rollback`。后两个阶段不变，依次调用 `POST /api/v1/releases/nginx/test`、`POST /api/v1/releases/nginx/reload`，成功均为 HTTP 200。阶段之间将节点地址、主机名和第一阶段返回的 `release_id` 保存到工作区进度文件。后两个接口只提交 `env` 和该 `release_id`。

同步接口若返回 HTTP 200 且 `status: skipped`，表示该节点已经是相同 commit_id 且此前已完成 reload；工作区的节点状态文件会标记该节点，后续 nginx -t 和 reload 两个阶段均明确跳过，不会重复执行命令。任一步失败即停止后续阶段；Nginx 检测、reload 或验证失败时，节点服务会恢复切换前的 latest。Jenkins 自身在阶段之间异常不会自动取消或暂停节点上的发布；下次发布可正常执行。

Git 仓库位置不作为 Jenkins 请求参数；服务按 type 选择 repos.config 或 repos.whitelist。

## 失败处理

Git 获取、latest 切换、nginx -t、reload、生效验证及恢复结果都会按 node_id 输出到控制台。配置内容有问题时修复 GitLab 提交后再跑 update；需要人工回退时选择 rollback，它只接受当前版本对应的上一成功快照，避免按目录修改时间误选版本。新发布会接管旧的待恢复记录，不需要人工解锁。白名单、前端发布和摘要固定均不走本 Job。
