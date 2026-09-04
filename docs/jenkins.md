# Jenkins HTTP 发布流水线

修订日期：2026-09-05。仓库根目录的 `Jenkinsfile` 用于配置/白名单发布。构建参数只保留 ACTION 和 SERVER_NAME；commit 使用当前 Job 检出的 GitLab 仓库 `GIT_COMMIT`。回滚只用于前端发布 Job，本流水线不提供 resume/rollback。

## Job 与执行环境

Job 使用 Pipeline from SCM，检出的 GitLab 仓库必须是本次要发布的配置或白名单制品仓库，这样 `GIT_COMMIT` 才是节点 `commit_id`。环境、类型、分支、部署目录、节点地址和凭据 ID 写在 `Jenkinsfile` 的 `environment` 中。白名单 Job 复制此流水线后把 `RELEASE_TYPE` 改为 `whitelist`。

执行器需要 Bash 和 Python 3.9+，能够访问节点 HTTP 地址。流水线使用 Pipeline 和 Credentials Binding。Jenkins 的 `agent any` 是执行器声明，节点发布通信使用 HTTP。

在 Jenkins 中保存 Secret text 凭据，ID 与 `RELEASE_CREDENTIAL_ID` 一致，内容为节点 `release_auth_tokens` 中对应环境的 Token。凭据仅注入 RELEASE_TOKEN，不写入批次 JSON。

## 构建参数与 HTTP 字段

| 来源 | HTTP 字段或用途 |
| --- | --- |
| ACTION（仅 update） | 新发布 |
| SERVER_NAME | params.server_name |
| 当前 Job 的 GIT_COMMIT | 顶层 commit_id |
| 当前 Job 的 GIT_BRANCH | 顶层 branch |
| environment 固定值 | env、type、project、path_dest、节点 URL、失败策略 stop |

Git 仓库位置不作为 Jenkins 请求参数；服务按 type 选择 repos.config 或 repos.whitelist。

## 失败处理

逐节点同步执行。检查/reload 错误通过 HTTP 返回；客户端以退出码 1 结束，流水线的 sh 直接失败，后续节点不再发布。配置/白名单失败后修复 GitLab 提交，再跑一次 update。前端回滚不走本 Job。
