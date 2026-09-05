// HTTP 发布流水线。Jenkins 的 agent 只指定构建执行器，节点部署用 curl 直连 HTTP。
// 本 Job 只发布 config / whitelist；前端制品请使用独立的 Jenkinsfile。
// 发布提交来自下方配置的 GitLab 仓库，而不是 Jenkinsfile 所在仓库。
pipeline {
  agent any
  options { timestamps(); disableConcurrentBuilds(); skipDefaultCheckout() }
  parameters {
    choice(name: 'ACTION', choices: ['update'], description: '配置/白名单发布')
    choice(name: 'RELEASE_TYPE', choices: ['config', 'whitelist'], description: '发布类型；不支持 frontend_static')
    choice(name: 'SERVER_NAME', choices: ['ybf-uat-nginx', 'jp-ybf-uat-nginx'], description: '配置中允许的站点')
  }
  environment {
    RELEASE_ENV = 'uat'
    RELEASE_BRANCH = 'uat'
    RELEASE_PROJECT = 'ybf'
    RELEASE_PATH_DEST = '/data/nginx-publish'
    // GitLab 制品仓库及其 Jenkins Username/Password 或 SSH 私钥凭据。
    RELEASE_SOURCE_REPOSITORY_URL = 'https://dcproopsgitlab.opscom999.com/dc-ops/nginx_vhost.git'
    RELEASE_SOURCE_CREDENTIAL_ID = 'gitlab_pushom'
    // 节点 HTTP 发布 Token（Secret text 凭据），与 GitLab 检出凭据不同。
    RELEASE_CREDENTIAL_ID = 'release-token-uat'
    SERVICE_URLS_ybf_uat_nginx = 'http://127.0.0.1:9166,http://127.0.0.1:9167'
    SERVICE_URLS_jp_ybf_uat_nginx = 'http://127.0.0.1:9168,http://127.0.0.1:9169'
  }
  stages {
    stage('Checkout release source') {
      steps {
        checkout([$class: 'GitSCM',
          branches: [[name: "*/${env.RELEASE_BRANCH}"]],
          doGenerateSubmoduleConfigurations: false,
          extensions: [],
          userRemoteConfigs: [[
            credentialsId: env.RELEASE_SOURCE_CREDENTIAL_ID,
            url: env.RELEASE_SOURCE_REPOSITORY_URL
          ]]
        ])
      }
    }
    stage('Prepare') {
      steps {
        script {
          if (!isUnix()) { error('发布执行器需要 curl，请使用 Linux 执行器') }
          env.RELEASE_TYPE = params.RELEASE_TYPE
          env.RELEASE_SERVER_NAME = params.SERVER_NAME
          env.RELEASE_URLS = env["SERVICE_URLS_${params.SERVER_NAME.replace('-', '_')}"]
          env.RELEASE_COMMIT = sh(script: 'git rev-parse HEAD', returnStdout: true).trim().toLowerCase()
          if (!(env.RELEASE_TYPE in ['config', 'whitelist'])) { error('RELEASE_TYPE 只能为 config 或 whitelist') }
          if (!(env.RELEASE_COMMIT ==~ /(?:[a-f0-9]{40}|[a-f0-9]{64})/)) {
            error('GitLab 制品仓库当前提交必须是完整 SHA')
          }
          if (!env.RELEASE_URLS?.trim()) { error('未配置该站点的节点 HTTP 地址') }
          writeFile file: 'release-request.json', text: groovy.json.JsonOutput.toJson([
            env: env.RELEASE_ENV,
            type: env.RELEASE_TYPE,
            branch: env.RELEASE_BRANCH,
            commit_id: env.RELEASE_COMMIT,
            project: env.RELEASE_PROJECT,
            params: [
              server_name: env.RELEASE_SERVER_NAME,
              path_dest: env.RELEASE_PATH_DEST
            ]
          ])
        }
      }
    }
    stage('同步 Git 并切换 latest') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          script {
            def urls = env.RELEASE_URLS.split(/[\s,]+/).findAll { it }
            if (!urls) { error('SERVICE_URLS 不能为空') }
            urls.eachWithIndex { url, index ->
              env.RELEASE_NODE_URL = url.replaceAll(/\/+$/, '')
              sh(script: """
                set -eu
                show_json() {
                  if command -v python3 >/dev/null 2>&1; then python3 -m json.tool < "\$1"; else cat "\$1"; fi
                }
                node="\$RELEASE_NODE_URL"
                echo "========== 同步 Git 并切换 latest：\$node =========="
                echo "GET \$node/healthz"
                health="\$(curl -sS --max-time 30 "\$node/healthz")"
                printf '%s' "\$health" > "sync-health-${index}.json"
                echo "\$health" | grep -Eq '"release_contract"[[:space:]]*:[[:space:]]*2' || { echo 'release_contract 必须为 2'; exit 1; }
                echo "\$health" | grep -Eq '"publish_ready"[[:space:]]*:[[:space:]]*true' || { echo '节点 publish_ready 不为 true'; exit 1; }
                node_id="\$(sed -nE 's/.*"node_id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\\1/p' "sync-health-${index}.json" | head -n 1)"
                test -n "\$node_id" || { echo '健康检查缺少 node_id'; exit 1; }
                printf '%s\n' "\$node_id" > "release-node-${index}.txt"
                echo "POST \$node/api/v1/releases/apply"
                code="\$(curl -sS -o "sync-response-${index}.json" -w '%{http_code}' --max-time 420 \\
                  -X POST "\$node/api/v1/releases/apply" \\
                  -H 'Content-Type: application/json' \\
                  -H "X-Release-Token: \$RELEASE_TOKEN" \\
                  --data-binary @release-request.json)"
                echo "节点主机：\$node_id  HTTP \$code"
                show_json "sync-response-${index}.json"
                if [ "\$code" = 200 ] && grep -Eq '"status"[[:space:]]*:[[:space:]]*"skipped"' "sync-response-${index}.json"; then
                  printf '%s\n' skipped > "release-status-${index}.txt"
                  : > "release-id-${index}.txt"
                  echo "\$node_id 已是目标 commit；nginx -t 和 reload 阶段将跳过。"
                  echo '=============================================='
                  exit 0
                fi
                test "\$code" = 202 || { echo "同步接口应返回 HTTP 202，实际为 \$code"; exit 1; }
                grep -Eq '"status"[[:space:]]*:[[:space:]]*"running"' "sync-response-${index}.json" || { echo '同步响应 status 不是 running'; exit 1; }
                grep -Eq '"phase"[[:space:]]*:[[:space:]]*"awaiting_nginx_test"' "sync-response-${index}.json" || { echo '同步响应未进入 awaiting_nginx_test'; exit 1; }
                release_id="\$(sed -nE 's/.*"release_id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\\1/p' "sync-response-${index}.json" | head -n 1)"
                echo "\$release_id" | grep -Eq '^[0-9a-fA-F-]{36}\$' || { echo '同步响应缺少合法 release_id'; exit 1; }
                printf '%s\n' "\$release_id" > "release-id-${index}.txt"
                printf '%s\n' waiting_test > "release-status-${index}.txt"
                echo "Git 更新、快照准备和 latest 切换已完成；release_id=\$release_id"
                echo '=============================================='
              """)
            }
          }
        }
      }
    }
    stage('nginx -t 配置检测') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          script {
            def urls = env.RELEASE_URLS.split(/[\s,]+/).findAll { it }
            if (!urls) { error('SERVICE_URLS 不能为空') }
            urls.eachWithIndex { url, index ->
              env.RELEASE_NODE_URL = url.replaceAll(/\/+$/, '')
              sh(script: """
                set -eu
                show_json() {
                  if command -v python3 >/dev/null 2>&1; then python3 -m json.tool < "\$1"; else cat "\$1"; fi
                }
                node_id="\$(cat "release-node-${index}.txt")"
                state="\$(cat "release-status-${index}.txt")"
                if [ "\$state" = skipped ]; then echo "\$node_id 已是目标 commit，跳过 nginx -t。"; exit 0; fi
                test "\$state" = waiting_test || { echo "节点 \$node_id 不在等待 nginx -t 状态：\$state"; exit 1; }
                release_id="\$(cat "release-id-${index}.txt")"
                echo "\$release_id" | grep -Eq '^[0-9a-fA-F-]{36}\$' || { echo '缺少合法 release_id'; exit 1; }
                printf '{"env":"%s","release_id":"%s"}' "\$RELEASE_ENV" "\$release_id" > "nginx-test-request-${index}.json"
                echo "========== nginx -t 配置检测：\$node_id (\$RELEASE_NODE_URL) =========="
                echo "POST \$RELEASE_NODE_URL/api/v1/releases/nginx/test"
                code="\$(curl -sS -o 'nginx-test-response-${index}.json' -w '%{http_code}' --max-time 120 \\
                  -X POST "\$RELEASE_NODE_URL/api/v1/releases/nginx/test" \\
                  -H 'Content-Type: application/json' \\
                  -H "X-Release-Token: \$RELEASE_TOKEN" \\
                  --data-binary @'nginx-test-request-${index}.json')"
                echo "节点主机：\$node_id  release_id=\$release_id  HTTP \$code"
                show_json "nginx-test-response-${index}.json"
                test "\$code" = 202 || { echo "nginx -t 接口应返回 HTTP 202，实际为 \$code"; exit 1; }
                grep -Eq '"status"[[:space:]]*:[[:space:]]*"running"' "nginx-test-response-${index}.json" || { echo 'nginx -t 响应 status 不是 running'; exit 1; }
                grep -Eq '"phase"[[:space:]]*:[[:space:]]*"awaiting_nginx_reload"' "nginx-test-response-${index}.json" || { echo 'nginx -t 未进入 awaiting_nginx_reload'; exit 1; }
                printf '%s\n' waiting_reload > "release-status-${index}.txt"
                echo 'nginx -t 检测完成，等待 reload。'
                echo '=============================================='
              """)
            }
          }
        }
      }
    }
    stage('nginx -s reload 与生效验证') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          script {
            def urls = env.RELEASE_URLS.split(/[\s,]+/).findAll { it }
            if (!urls) { error('SERVICE_URLS 不能为空') }
            urls.eachWithIndex { url, index ->
              env.RELEASE_NODE_URL = url.replaceAll(/\/+$/, '')
              sh(script: """
                set -eu
                show_json() {
                  if command -v python3 >/dev/null 2>&1; then python3 -m json.tool < "\$1"; else cat "\$1"; fi
                }
                node_id="\$(cat "release-node-${index}.txt")"
                state="\$(cat "release-status-${index}.txt")"
                if [ "\$state" = skipped ]; then echo "\$node_id 已是目标 commit，跳过 nginx -s reload。"; exit 0; fi
                test "\$state" = waiting_reload || { echo "节点 \$node_id 不在等待 reload 状态：\$state"; exit 1; }
                release_id="\$(cat "release-id-${index}.txt")"
                echo "\$release_id" | grep -Eq '^[0-9a-fA-F-]{36}\$' || { echo '缺少合法 release_id'; exit 1; }
                printf '{"env":"%s","release_id":"%s"}' "\$RELEASE_ENV" "\$release_id" > "nginx-reload-request-${index}.json"
                echo "========== nginx -s reload 与生效验证：\$node_id (\$RELEASE_NODE_URL) =========="
                echo "POST \$RELEASE_NODE_URL/api/v1/releases/nginx/reload"
                code="\$(curl -sS -o 'nginx-reload-response-${index}.json' -w '%{http_code}' --max-time 120 \\
                  -X POST "\$RELEASE_NODE_URL/api/v1/releases/nginx/reload" \\
                  -H 'Content-Type: application/json' \\
                  -H "X-Release-Token: \$RELEASE_TOKEN" \\
                  --data-binary @'nginx-reload-request-${index}.json')"
                echo "节点主机：\$node_id  release_id=\$release_id  HTTP \$code"
                show_json "nginx-reload-response-${index}.json"
                test "\$code" = 200 || { echo "reload 接口应返回 HTTP 200，实际为 \$code"; exit 1; }
                grep -Eq '"status"[[:space:]]*:[[:space:]]*"succeeded"' "nginx-reload-response-${index}.json" || { echo 'reload 响应 status 不是 succeeded'; exit 1; }
                printf '%s\n' complete > "release-status-${index}.txt"
                echo 'nginx -s reload 与生效验证完成。'
                echo '=============================================='
              """)
            }
          }
        }
      }
    }
  }
  post {
    success { echo 'HTTP 发布完成。' }
    failure {
      withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
        script {
          def urls = env.RELEASE_URLS?.split(/[\s,]+/)?.findAll { it } ?: []
          urls.eachWithIndex { url, index ->
            env.RELEASE_NODE_URL = url.replaceAll(/\/+$/, '')
            sh(script: """
              set +e
              status_file="release-status-${index}.txt"
              id_file="release-id-${index}.txt"
              if [ ! -f "\$status_file" ] || [ ! -f "\$id_file" ]; then
                echo "未找到 \$RELEASE_NODE_URL 的暂挂发布记录，无需自动取消。"
                exit 0
              fi
              state="\$(cat "\$status_file")"
              if [ "\$state" != waiting_test ] && [ "\$state" != waiting_reload ]; then
                echo "\$RELEASE_NODE_URL 当前状态为 \$state，无需自动取消。"
                exit 0
              fi
              release_id="\$(cat "\$id_file")"
              echo "流水线失败，自动恢复 \$RELEASE_NODE_URL 的 release_id=\$release_id"
              printf '{"env":"%s","release_id":"%s"}' "\$RELEASE_ENV" "\$release_id" > "release-abort-request-${index}.json"
              code="\$(curl -sS -o "release-abort-response-${index}.json" -w '%{http_code}' --max-time 120 \\
                -X POST "\$RELEASE_NODE_URL/api/v1/releases/abort" \\
                -H 'Content-Type: application/json' \\
                -H "X-Release-Token: \$RELEASE_TOKEN" \\
                --data-binary @"release-abort-request-${index}.json")"
              echo "自动恢复接口 HTTP \$code"
              if [ -f "release-abort-response-${index}.json" ]; then
                if command -v python3 >/dev/null 2>&1; then python3 -m json.tool < "release-abort-response-${index}.json"; else cat "release-abort-response-${index}.json"; fi
              fi
              exit 0
            """)
          }
        }
      }
      echo '发布失败，具体 Git、nginx -t、reload 及自动恢复结果见上方控制台。若节点仍为 recovery_required，请先恢复该节点再重新发布。'
    }
  }
}
