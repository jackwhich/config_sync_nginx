# 前端：ORAS 投递 dist.tar.gz

修订日期：2026-09-04。CI 经 HTTPS 代理向 Harbor 推送 OCI 文件制品；云主机上的 Go HTTP 服务调用独立 ORAS 二进制拉取、解包、原子切换软链，再执行已有的 `nginx -t`，通过后执行 `nginx -s reload`。整个流程不需要 Docker daemon，也不运行容器。

按本次目录约定，实际布局为：

```text
<path_dest>/<server_name>/                 # 例如 /var/www/app
  <完整 git SHA>/                         # 40 或 64 位，直接存放制品
    index.html
    assets/...
    .release-version                     # 服务生成的版本探针
  latest -> <完整 git SHA>                # latest 和 SHA 目录同级
  .publisher.json                        # 发布目录归属
  .staging/stage-<UUID>/                  # 随机临时快照
  .manifests/<完整 git SHA>.json          # 服务生成的完整文件摘要
```

没有中间的 releases 目录，也不使用 current。配置和白名单的既有 Git 发布目录保持 `<path_dest>/<type>/<server_name>/releases/<SHA>` + latest。

## 制品身份与契约

| 项 | 约定 |
| --- | --- |
| 可读坐标 | `harbor.example.com/<项目>/<应用>-dist:<完整 git SHA>` |
| 机器拉取坐标 | 同一仓库的 `@sha256:<OCI manifest digest>` |
| 人工展示 | 发布成功后移动 `prod` tag，机器不以 prod 为发布依据 |
| artifact type | `application/vnd.nginx.frontend.dist.v1` |
| OCI annotation | `org.opencontainers.image.revision=<完整 git SHA>` |
| 必须的 layer | `dist.tar.gz`，media type 为 `application/gzip` |
| 可选 layer | `dist.tar.gz.sha256`（text/plain）、`manifest.json`（application/json） |
| tar 根目录 | 直接是非空 index.html 和静态资源，不再套一层 dist/ |

Git SHA 标识构建源码，OCI digest 标识这一次确切制品。服务检查 manifest 自身摘要、revision annotation、每个文件 layer 的大小与 SHA-256，解包后再生成文件清单。同一 Git SHA 不能用于替换本机已存在的不同内容快照；Harbor 的 SHA tag 也应设置为不可变，prod 除外。

可选校验文件格式：`<tar 的 SHA256>  dist.tar.gz`。可选 manifest.json 格式为 `{"commit_id":"<完整SHA>","sha256":"<tar的64位SHA256>"}`。两者都不是 OCI manifest；推送脚本导出的 oci-manifest.json 是 OCI 元数据，不作为 layer 再推送。

服务只接受上述有限 layer 文件名，拒绝 ORAS 自动解包注解、目录 layer 或任意输出路径。tar 解包支持正常 `./index.html`，拒绝路径穿越、绝对路径、软硬链接、特殊文件、重复文件、压缩损坏及数量/体积超限；包内 `.release-version` 由服务保留，不能自行提供。

## CI：构建、打包、推送

Linux CI 安装 Node/npm、tar、sha256sum 和 ORAS 1.3.x。首先在前端源码目录执行构建；任何一步失败均停止：

```bash
npm ci
npm run build
export RELEASE_COMMIT="$(git rev-parse HEAD)"
export HARBOR_REPOSITORY=harbor.example.com/web/app-dist
export HTTPS_PROXY=http://proxy.internal:8080
export NO_PROXY=localhost,127.0.0.1,.internal
# HARBOR_USERNAME/HARBOR_PASSWORD 由 CI 凭据注入，用户名可含 $，不要直接写进脚本。
bash /path/to/config_sync_nginx/scripts/frontend-artifact.sh push dist ../artifact-bundle
```

脚本先确认 index.html、完成 tar 和摘要，再登录并 push；输出目录必须是新的且位于 dist 外。密码通过标准输入，临时认证文件退出时清理。NO_PROXY 不应匹配需要经代理访问的 Harbor。

脚本等价的 ORAS 关键命令为：

```bash
# 在存放 dist.tar.gz 的目录中执行
oras push "$HARBOR_REPOSITORY:$RELEASE_COMMIT" \
  --artifact-type application/vnd.nginx.frontend.dist.v1 \
  --annotation "org.opencontainers.image.revision=$RELEASE_COMMIT" \
  --export-manifest oci-manifest.json \
  dist.tar.gz:application/gzip dist.tar.gz.sha256:text/plain
```

脚本从实际推送的 OCI manifest 计算摘要，输出 `artifact.digest` 和 `artifact.ref`。将制品摘要和 HTTP 批次记录一起归档；不在 push 成功后立即改 prod。

## 云主机：一次性配置

云主机沿用已有独立 Nginx，准备 ORAS 1.3.x 和 Go 编译得到的发布服务。使用仅有目标仓库 pull 权限的 Robot 预置认证文件：

```bash
install -d -m 0700 /etc/nginx-release
umask 077
# 下列变量由运维凭据注入；避免 -p 将密码放到命令参数。
printf '%s' "$HARBOR_PULL_PASSWORD" | /usr/local/bin/oras login harbor.example.com \
  -u "$HARBOR_PULL_USERNAME" --password-stdin \
  --registry-config /etc/nginx-release/harbor-auth.json
```

凭据文件由服务账号读取并限制为 0600。服务配置仓库白名单，HTTP 请求不能指定任意 Harbor 地址，也不接收 registry 密码。主机 ORAS 子进程显式清空大小写 HTTP/HTTPS/ALL_PROXY 并设置 NO_PROXY=*；不会继承 CI 代理或启用 ORAS 磁盘缓存。私有 CA 使用 `oras.ca_file`，不关闭 TLS 校验。

完整服务配置见 `configs/frontend-service.example.yaml`。仓库地址集中配置，站点和部署路径由 HTTP 请求传入：

```yaml
oras:
  binary: /usr/local/bin/oras
  registry_config: /etc/nginx-release/harbor-auth.json
  repository: harbor.example.com/web/{server_name}-dist
targets:
  - frontend_static
```

`{server_name}` 替换为 params 中的站点名；也可写完整固定仓库名。Nginx 已有 root 指向 `/var/www/app/latest`，示例见 `configs/frontend-location.example.conf`。服务无需 nginx 路径块，调用已有 `nginx -t`，通过后执行 `nginx -s reload` 并验证本地文件；可选 HTTP 探测通过高级逐站点配置启用，见 [请求配置约定](request-targets.md)。

## HTTP 发布与恢复

协议主版本保持 2；前端客户端额外要求 healthz 的 capabilities 包含 `frontend_oras_v1`。缺少此能力时，在任何 POST 之前停止，避免把新请求发给旧 Git 前端服务。

前端请求只需完整 commit_id 和站点参数。artifact_digest 可选：不传时读取完整 SHA tag 的 manifest，验证后计算 digest 并按该摘要 pull；此能力要求 `request_targets_v1`。branch 可省略。下面展示带摘要和并发修订号的完整请求：

```json
{
  "release_id": "70a0aa0b-ed68-4abf-9f76-58fe71777dfe",
  "expected_state_revision": "e2f5a517-dc90-4258-ae0a-9ab4eb319626",
  "env": "uat",
  "type": "frontend_static",
  "commit_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "artifact_digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "params": {"server_name":"app","path_dest":"/var/www"}
}
```

ID、SHA 和 digest 都是占位值。使用既有批量客户端时：

```bash
export RELEASE_URLS='http://node-a:9166,http://node-b:9166'
export RELEASE_ENV=uat RELEASE_TYPE=frontend_static
export RELEASE_SERVER_NAME=app RELEASE_PATH_DEST=/var/www
# 可选：应用 CI 已持有摘要时直接指定。留空由首台节点解析并固定后续节点摘要。
# export RELEASE_ARTIFACT_DIGEST="$(cat ../artifact-bundle/artifact.digest)"
# RELEASE_COMMIT 是构建前保存的完整 SHA；RELEASE_TOKEN 从 CI 凭据注入。
bash scripts/release-apply.sh update --batch-file release-batch-frontend.json
# 断线：使用原记录和原 UUID 查询/恢复，不创建新批次。
bash scripts/release-apply.sh resume --batch-file release-batch-frontend.json
# 恢复：按每个节点自己的本地基线及原 artifact_digest 执行。
bash scripts/release-apply.sh rollback --batch-file release-batch-frontend.json
```

仓库根目录 [Jenkinsfile](../Jenkinsfile) 用于配置/白名单 update，不承担前端回滚。前端 Harbor pull 地址保留在节点配置；应用 CI 完成构建和 push 后，用 HTTP 客户端或单独的前端 Job 发布。省略摘要时客户端固定首台返回摘要后再发布后续节点。

| 顺序 | 动作 | 失败处理 |
| --- | --- | --- |
| 1 | CI build + tar + checksum | 不 login、不 push |
| 2 | CI 经代理 push SHA tag，保存 manifest digest | 不触发节点发布、不改 prod |
| 3 | 预检全部节点、保存请求 UUID 与各节点基线 | 不发送发布请求 |
| 4 | 节点查验固定 digest manifest，oras pull 到独立临时目录，检查与解包 | 删除临时半包，latest 不变 |
| 5 | 建立 `<server_name>/<完整SHA>` 不可变快照，持久化切换意图 | latest 不变 |
| 6 | 新建随机临时软链，原子 rename 为 latest；执行 nginx -t，通过后 nginx -s reload，校验本地文件（以及可选 HTTP 探测） | 切回本节点旧快照，再次执行 nginx -t，通过后 nginx -s reload 并校验旧快照 |
| 7 | 原子提交状态和发布结果 | 不确定时标记 recovery_required，停止后续发布 |
| 8 | 全部节点成功后，CI 更新展示用 prod tag | 仅展示指针滞后，记录告警并修复 |

临时软链和 latest 位于同一父目录；服务用 rename 原子替换，不使用可能出现中间缺口的多条 ln/rm 操作。一次发布失败的自动恢复不访问 Harbor；人工撤销批次使用 restore_of，并校验原成功后的 revision，禁止覆盖别人之后的发布。首次部署无旧快照时使用 stop 策略；失败删除新 latest 后执行 nginx -t，通过后再次执行 nginx -s reload；配置了 initial_health_checks 时验证原入口。

## prod 的意义与权限

只有在全部预定节点发布成功后，CI 才执行：

```bash
oras tag "$HARBOR_REPOSITORY@$RELEASE_ARTIFACT_DIGEST" prod
```

`oras tag` 的第二个参数是新 tag 名 `prod`，不是完整仓库坐标。CI 应串行化同一应用的部署与 prod 更新，避免并发批次把展示指针写回旧版本。混合版本或部分回滚时，一个 prod tag 无法表达各节点实际状态，以 HTTP state 为准；不得把 prod 当发布成功证据。完整回滚后仅当所有节点基线 digest 一致时才有唯一的旧 prod 可恢复。

CI Robot 需要指定项目的 pull/push 与 tag 权限（Harbor 的 push 需同时授予 pull）；云主机 Robot 仅需 pull。人仅查看 prod。[Harbor Robot 权限依据](https://goharbor.io/docs/2.14.0/working-with-projects/project-configuration/create-robot-accounts/)。Harbor 的 SHA tag 不可变策略、保留规则以及 Robot 权限由仓库管理员配置；GC 不应删除仍需部署的摘要。

## 缓存与旧前端目录迁移

默认 shared_assets=false 可投递普通构建 dist，不要求 Python 或 frontend-manifest.json；HTML/版本探针不缓存。latest 切换后，旧页面再请求仅存在于旧版本的资源可能 404，即使旧 SHA 目录还在磁盘也不会自动路由过去。有长时间打开页面和懒加载需求时，启用 shared_assets=true 并配置独立共享 /assets 路由，沿用哈希资源清单校验及 asset_retention 窗口；这才需要可选的 frontend-manifest.py 工具。

旧 `<path_dest>/frontend_static/<server_name>/releases/...` 目录和 Git 来源状态不自动认领为新 ORAS 状态。先在新空目录和独立验证入口试发布，保留旧目录，检查生产 Nginx root 切换方案；不要手改状态文件或 `.publisher.json`。服务遇到已有未管理文件会要求明确迁移。

已通过测试覆盖 manifest/文件摘要错误、revision 不匹配、危险 layer、gzip 损坏、tar 穿越/链接/限额、SHA 目录布局、摘要冲突、失败不切换、离线恢复和客户端能力预检。真实 Harbor Robot/代理/TLS 以及实际 Nginx 实例仍需部署环境联调；注入 ORAS 返回结果的测试不能替代真实网络验收。

命令依据：[ORAS push](https://oras.land/docs/commands/oras_push/)、[pull](https://oras.land/docs/commands/oras_pull/)、[manifest fetch](https://oras.land/docs/commands/oras_manifest_fetch/)、[tag](https://oras.land/docs/commands/oras_tag/)。
