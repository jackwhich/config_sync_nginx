# Jenkins HTTP 发布流水线

修订日期：2026-09-04。仓库根目录的 `Jenkinsfile` 用于把配置、白名单或已推送 Harbor 的前端制品发布到 HTTP 节点。参数由本次构建传入，节点上的 Git/Harbor 连接信息和 data_dir 保留在服务配置中。

## Job 与执行环境

Job 使用 Pipeline from SCM，读取本仓库 `master` 分支的 `Jenkinsfile`。这里的 master 是流水线代码分支；下方 BRANCH 是配置/白名单制品仓库的分支，两者独立。

执行器需要 Bash 和 Python 3.9+，能够访问节点 HTTP 地址。流水线使用 Pipeline 和 Credentials Binding；resume/rollback 额外使用 Copy Artifact 插件读取本 Job 的历史构建归档。Jenkins 的 `agent any` 是执行器声明，节点发布通信使用 HTTP。

在 Jenkins 中保存 Secret text 凭据，内容为节点 `release_auth_tokens` 中对应环境的 Token。构建参数填写该凭据的 ID，不填写明文 Token。凭据仅注入 RELEASE_TOKEN，不写入批次 JSON。

## 构建参数与 HTTP 字段

| Jenkins 参数 | HTTP 字段或用途 |
| --- | --- |
| ACTION | update 新发布；resume 继续原批次；rollback 恢复原批次 |
| DEPLOY_ENV | 顶层 env，默认 uat，可修改；恢复时必须匹配原批次 |
| RELEASE_TYPE | 顶层 type：config / whitelist / frontend_static |
| BRANCH | Git 发布的顶层 branch，可省略；frontend_static 不发送该字段 |
| COMMIT_ID | 顶层 commit_id，完整 40/64 位 SHA，统一小写 |
| SERVER_NAME | params.server_name，任意符合服务校验的站点目录键 |
| PATH_DEST | params.path_dest，节点上的绝对部署根目录 |
| SERVICE_URLS | 节点 HTTP 地址，按逗号、空格或换行拆分，顺序发布 |
| PROJECT | 可选顶层 project |
| ARTIFACT_DIGEST | 前端可选 artifact_digest；格式 sha256:加 64 位十六进制摘要 |
| TOKEN_CREDENTIAL_ID | 对应环境的 Jenkins Secret text 凭据 ID |
| SOURCE_BUILD | resume/rollback 使用的本 Job 原构建编号 |
| FAILURE_POLICY | 新批次失败策略；stop 或 restore，首次发布使用 stop |

update 必须填写 COMMIT_ID、SERVER_NAME、PATH_DEST、SERVICE_URLS、DEPLOY_ENV 和凭据 ID。Git 仓库位置不作为 Jenkins 请求参数；服务按 type 选择 repos.config 或 repos.whitelist。前端 Harbor pull 地址只从服务 oras.repository 读取。

配置发布例：DEPLOY_ENV=uat、RELEASE_TYPE=config、BRANCH=uat、SERVER_NAME=app、PATH_DEST=/data/nginx-publish，COMMIT_ID 填配置仓库的完整提交。白名单选择 whitelist，其余参数按实际请求填写。

前端发布例：RELEASE_TYPE=frontend_static、SERVER_NAME=app、PATH_DEST=/var/www，COMMIT_ID 填应用构建的完整 SHA。落盘为 `/var/www/app/<完整 SHA>`，同级 latest 指向该 SHA。

## 前端摘要与构建边界

应用 CI 先完成 npm 构建，再经代理调用 `scripts/frontend-artifact.sh push` 推送 dist.tar.gz，见 [ORAS 流程](frontend-oras.md)。本 Jenkinsfile 负责后续 HTTP 发布，不在该发布 Job 中重新检出、构建前端，也不从 Jenkins 直接向主机复制 dist。

ARTIFACT_DIGEST 可使用应用 CI 产出的 artifact.digest；留空时，所有节点预检必须支持 request_targets_v1。首台节点按完整 SHA tag 解析并返回确切摘要；客户端先把摘要写入批次，再注入尚未发布节点的请求。后续节点按同一摘要 pull，不能各自重新解析移动标签。首台请求保持原参数，以便断线后按原 UUID 查询；缺少摘要或摘要不一致会失败并停止后续发布。

prod 只是展示标签，本发布 Job 不更新它。应用 CI 如需更新 prod，应在 HTTP 批次全部成功后，使用实际发布摘要执行标记；不能在构建或 push 后立即把 prod 当成已发布版本。

## 失败、继续与回滚

逐节点同步执行：准备文件、切换候选 latest、nginx -t，通过后 nginx -s reload，再确认发布结果。检查/reload 错误通过 HTTP 返回；客户端输出 error_code 和 error、保存批次、以退出码 1 结束。流水线的 sh 直接传播该失败，后续节点不再发布。选择 restore 时，已成功节点恢复自己的旧基线，但本次构建仍失败。

批次归档路径是 `http-release-<本构建编号>/release-batch.json`。resume/rollback 指定 SOURCE_BUILD 后，复制这一构建中的批次 JSON。类型、分支、SHA、站点、目录、节点地址、摘要、失败策略和 UUID 均使用原记录，不用新填写的发布参数覆盖。DEPLOY_ENV 不匹配时在发送 HTTP 前拒绝；Token 可以重新选择同一环境当前有效的凭据。

首次部署没有旧基线时不能使用 restore。未知结果须先用 resume 查询原 UUID；不得为了重试而直接重新发起新 update。

## 验证范围

客户端测试覆盖三种类型的参数传递、可选 Git 分支、前端 SHA 解析与跨节点摘要固定、断点继续和逐节点恢复、环境不匹配拒绝，以及 nginx 错误导致退出码 1 和停止后续节点。Jenkinsfile 已按官方声明式 Pipeline 语法和步骤定义核对；尚未连接实际 Jenkins 实例执行 Linter 或 Job，目标实例的插件、凭据权限和节点网络仍需现场验证。
