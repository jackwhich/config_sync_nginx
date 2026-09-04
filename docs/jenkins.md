# Jenkins HTTP 发布流水线

修订日期：2026-09-05。仓库根目录的 `Jenkinsfile` 只用于配置/白名单发布；不接受 `frontend_static`。它显式检出 GitLab 制品仓库的 `uat` 分支，使用该工作区的 HEAD 作为 `commit_id`。流水线用 curl 直连节点 HTTP，不调用 `scripts/release-apply.sh`。前端制品应使用独立 Jenkinsfile 和 Job。

## Job 与执行环境

Job 可以使用 Pipeline from SCM 读取本仓库的 `Jenkinsfile`；流水线会跳过默认检出，再用 Git 插件显式检出制品仓库 `https://dcproopsgitlab.opscom999.com/dc-ops/nginx_vhost.git` 的 `uat` 分支。因此发布使用的是制品仓库的 HEAD，而非 Jenkinsfile 所在仓库的 SHA。`RELEASE_SOURCE_CREDENTIAL_ID=gitlab_pushom` 是该 GitLab 仓库的 Jenkins 凭据 ID；`RELEASE_CREDENTIAL_ID=release-token-uat` 则是节点 HTTP 发布 Token，两者不可混用。

环境、分支、部署目录、节点地址和凭据 ID 写在 `Jenkinsfile` 的 `environment` 中。构建时选择 `RELEASE_TYPE=config` 或 `RELEASE_TYPE=whitelist`。所选类型必须对应节点服务配置中的 `repos.config` 或 `repos.whitelist`；若两类制品位于不同 GitLab 仓库，应为白名单 Job 配置匹配的仓库地址和凭据。

执行器需要 curl，能够访问节点 HTTP 地址。流水线使用 Pipeline 和 Credentials Binding。Jenkins 的 `agent any` 是执行器声明，节点发布通信使用 HTTP。

在 Jenkins 中保存 Secret text 凭据，ID 与 `RELEASE_CREDENTIAL_ID` 一致，内容为节点 `release_auth_tokens` 中对应环境的 Token。凭据只放进 `X-Release-Token` 请求头。

## 构建参数与 HTTP 字段

| 来源 | HTTP 字段或用途 |
| --- | --- |
| ACTION（仅 update） | 新发布 |
| RELEASE_TYPE | 顶层 type：config 或 whitelist |
| SERVER_NAME | params.server_name |
| `nginx_vhost.git` 的 `uat` 分支 HEAD | 顶层 commit_id |
| `RELEASE_BRANCH`（默认 uat） | 顶层 branch |
| environment 固定值 | env、project、path_dest、节点 URL、GitLab 地址及凭据 ID |

每个节点先 `GET /healthz`，确认 `release_contract` 为 2 且 `publish_ready` 为 true，再 `POST /api/v1/releases/apply`。任一节点非 200 即停止后续节点。

Git 仓库位置不作为 Jenkins 请求参数；服务按 type 选择 repos.config 或 repos.whitelist。

## 失败处理

检查/reload 错误在 HTTP 响应体里，控制台会打印。配置/白名单失败后修复 GitLab 提交，再跑一次 update。前端发布、摘要固定和回滚均不走本 Job。
