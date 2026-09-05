// HTTP 发布流水线。Jenkins 的 agent 只指定构建执行器，节点部署用 curl 直连 HTTP。
// 本 Job 只发布 config；白名单和前端制品请使用各自独立的 Jenkinsfile。
// 发布提交来自下方配置的 GitLab 仓库，而不是 Jenkinsfile 所在仓库。
def jsonLogField(text, field) {
  def found = text =~ /"${field}"\s*:\s*"([^"]*)"/
  return found.find() ? found.group(1) : '-'
}

def jsonLogDuration(text) {
  def found = text =~ /"duration_ms"\s*:\s*(\d+)/
  return found.find() ? found.group(1) : '0'
}

// Keep console rendering independent of Python, jq, and Jenkins JSON plugins.
// Responses are service-generated JSON; the small field extraction here is only
// for human-readable logging and never controls a release decision.
def showReleaseLog(title, target, node, httpCode, text, taskSpecs) {
  try {
    def status = jsonLogField(text, 'status')
    def phase = jsonLogField(text, 'phase')
    def activation = jsonLogField(text, 'activation_status')
    def error = jsonLogField(text, 'error')
    echo ''
    echo "PLAY [${target}] ********************************************************"
    echo ''
    taskSpecs.each { spec ->
      echo "TASK [nginx-release : ${spec.title}] ****************************************"
      def matched = text =~ /(?s)\{[^{}]*"name"\s*:\s*"${spec.name}"[^{}]*\}/
      if (matched.find()) {
        def step = matched.group(0)
        def stepStatus = jsonLogField(step, 'status')
        def prefix = stepStatus == 'succeeded' ? 'ok' : (stepStatus == 'skipped' ? 'skipped' : 'failed')
        echo "${prefix}: [${node}] => status=${stepStatus} duration_ms=${jsonLogDuration(step)}"
        def message = jsonLogField(step, 'message')
        if (message != '-') { echo "  msg: ${message}" }
      } else {
        echo "skipped: [${node}] => not executed"
      }
      echo ''
    }
    def resultPrefix = status == 'succeeded' || status == 'skipped' ? 'ok' : 'failed'
    echo "${resultPrefix}: [${node}] => HTTP ${httpCode} status=${status} phase=${phase} activation=${activation}"
    if (error != '-') { echo "  msg: ${error}" }
    echo '************************************************************************'
    if (resultPrefix == 'failed') { echo "raw_response: ${text}" }
  } catch (Exception ignored) {
    // A log renderer must never turn a completed release into a failed build.
    echo "${title}: [${node}] HTTP ${httpCode}"
    echo text
  }
}

pipeline {
  agent any
  options { timestamps(); disableConcurrentBuilds(); skipDefaultCheckout() }
  parameters {
    choice(name: 'ACTION', choices: ['update', 'rollback'], description: 'update 发布当前 Git 提交；rollback 回退到当前版本的上一快照')
    choice(name: 'SERVER_NAME', choices: ['ybf-uat-nginx', 'jp-ybf-uat-nginx'], description: '配置中允许的站点')
  }
  environment {
    RELEASE_ENV = 'uat'
    RELEASE_BRANCH = 'uat'
    RELEASE_PROJECT = 'ybf'
    RELEASE_TYPE = 'config'
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
      when { expression { params.ACTION == 'update' } }
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
          if (!(params.ACTION in ['update', 'rollback'])) { error('ACTION 只能为 update 或 rollback') }
          env.RELEASE_ACTION = params.ACTION
          env.RELEASE_SERVER_NAME = params.SERVER_NAME
          env.RELEASE_URLS = env["SERVICE_URLS_${params.SERVER_NAME.replace('-', '_')}"]
          if (env.RELEASE_TYPE != 'config') { error('本 Job 仅支持 RELEASE_TYPE=config') }
          if (!env.RELEASE_URLS?.trim()) { error('未配置该站点的节点 HTTP 地址') }
          if (env.RELEASE_ACTION == 'update') {
            env.RELEASE_COMMIT = sh(script: 'git rev-parse HEAD', returnStdout: true).trim().toLowerCase()
            if (!(env.RELEASE_COMMIT ==~ /(?:[a-f0-9]{40}|[a-f0-9]{64})/)) {
              error('GitLab 制品仓库当前提交必须是完整 SHA')
            }
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
    }
    stage('同步 Git 并切换 latest') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          script {
            def urls = env.RELEASE_URLS.split(/[\s,]+/).findAll { it }
            if (!urls) { error('SERVICE_URLS 不能为空') }
            urls.eachWithIndex { url, index ->
              env.RELEASE_NODE_URL = url.replaceAll(/\/+$/, '')
              try {
                sh(script: """#!/bin/sh
                set -eu
                node="\$RELEASE_NODE_URL"
                health="\$(curl -sS --max-time 30 "\$node/healthz")"
                printf '%s' "\$health" > "sync-health-${index}.json"
                echo "\$health" | grep -Eq '"release_contract"[[:space:]]*:[[:space:]]*2' || { echo 'release_contract 必须为 2'; exit 1; }
                echo "\$health" | grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' || { echo '节点健康检查未返回 ok'; exit 1; }
                node_id="\$(sed -nE 's/.*"node_id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\\1/p' "sync-health-${index}.json" | head -n 1)"
                test -n "\$node_id" || { echo '健康检查缺少 node_id'; exit 1; }
                printf '%s\n' "\$node_id" > "release-node-${index}.txt"
                endpoint="\$node/api/v1/releases/apply"
                request_file='release-request.json'
                if [ "\$RELEASE_ACTION" = rollback ]; then
                  endpoint="\$node/api/v1/releases/rollback"
                  request_file="rollback-request-${index}.json"
                  printf '{"env":"%s","type":"%s","project":"%s","params":{"server_name":"%s","path_dest":"%s"}}' \
                    "\$RELEASE_ENV" "\$RELEASE_TYPE" "\$RELEASE_PROJECT" "\$RELEASE_SERVER_NAME" "\$RELEASE_PATH_DEST" > "\$request_file"
                fi
                code="\$(curl -sS -o "sync-response-${index}.json" -w '%{http_code}' --max-time 420 \\
                  -X POST "\$endpoint" \\
                  -H 'Content-Type: application/json' \\
                  -H "X-Release-Token: \$RELEASE_TOKEN" \\
                  --data-binary @"\$request_file")"
                printf '%s\n' "\$code" > "sync-http-${index}.txt"
                if [ "\$code" = 200 ] && grep -Eq '"status"[[:space:]]*:[[:space:]]*"skipped"' "sync-response-${index}.json"; then
                  printf '%s\n' skipped > "release-status-${index}.txt"
                  : > "release-id-${index}.txt"
                  exit 0
                fi
                test "\$code" = 200 || { echo "同步接口应返回 HTTP 200，实际为 \$code"; exit 1; }
                grep -Eq '"status"[[:space:]]*:[[:space:]]*"succeeded"' "sync-response-${index}.json" || { echo '同步响应 status 不是 succeeded'; exit 1; }
                release_id="\$(sed -nE 's/.*"release_id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\\1/p' "sync-response-${index}.json" | head -n 1)"
                echo "\$release_id" | grep -Eq '^[0-9a-fA-F-]{36}\$' || { echo '同步响应缺少合法 release_id'; exit 1; }
                printf '%s\n' "\$release_id" > "release-id-${index}.txt"
                phase="\$(sed -nE 's/.*"phase"[[:space:]]*:[[:space:]]*"([^"]+)".*/\\1/p' "sync-response-${index}.json" | head -n 1)"
                case "\$phase" in
                  latest_switched|nginx_test)
                    printf '%s\n' git_switched > "release-status-${index}.txt"
                    ;;
                  nginx_test_succeeded|reload|verify_activation)
                    printf '%s\n' nginx_test_succeeded > "release-status-${index}.txt"
                    ;;
                  *) echo "同步响应处于不支持的阶段：\$phase"; exit 1 ;;
                esac
                """)
              } finally {
                if (fileExists("sync-response-${index}.json")) {
                  def actionTitle = env.RELEASE_ACTION == 'rollback' ? '回滚至上一版本并切换 latest' : '同步 Git 并切换 latest'
                  def actionTasks = env.RELEASE_ACTION == 'rollback' ? [
                    [title: '读取当前版本与上一快照', name: 'verify_baseline'],
                    [title: '校验回滚快照', name: 'prepare_snapshot'],
                    [title: '切换 latest', name: 'switch']
                  ] : [
                    [title: "同步 Git（branch=${env.RELEASE_BRANCH}）", name: 'fetch'],
                    [title: '准备配置快照', name: 'prepare_snapshot'],
                    [title: '切换 latest', name: 'switch']
                  ]
                  showReleaseLog(actionTitle, "${env.RELEASE_TYPE}:${env.RELEASE_SERVER_NAME}",
                    fileExists("release-node-${index}.txt") ? readFile("release-node-${index}.txt").trim() : env.RELEASE_NODE_URL,
                    fileExists("sync-http-${index}.txt") ? readFile("sync-http-${index}.txt").trim() : '-',
                    readFile("sync-response-${index}.json"), actionTasks)
                }
              }
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
              try {
                sh(script: """#!/bin/sh
                set -eu
                rm -f "nginx-test-response-${index}.json" "nginx-test-http-${index}.txt"
                node_id="\$(cat "release-node-${index}.txt")"
                state="\$(cat "release-status-${index}.txt")"
                if [ "\$state" = skipped ]; then exit 0; fi
                test "\$state" = git_switched || { echo "节点 \$node_id 未完成 Git 切换：\$state"; exit 1; }
                release_id="\$(cat "release-id-${index}.txt")"
                echo "\$release_id" | grep -Eq '^[0-9a-fA-F-]{36}\$' || { echo '缺少合法 release_id'; exit 1; }
                printf '{"env":"%s","release_id":"%s"}' "\$RELEASE_ENV" "\$release_id" > "nginx-test-request-${index}.json"
                code="\$(curl -sS -o 'nginx-test-response-${index}.json' -w '%{http_code}' --max-time 120 \\
                  -X POST "\$RELEASE_NODE_URL/api/v1/releases/nginx/test" \\
                  -H 'Content-Type: application/json' \\
                  -H "X-Release-Token: \$RELEASE_TOKEN" \\
                  --data-binary @'nginx-test-request-${index}.json')"
                printf '%s\n' "\$code" > "nginx-test-http-${index}.txt"
                test "\$code" = 200 || { echo "nginx -t 接口应返回 HTTP 200，实际为 \$code"; exit 1; }
                grep -Eq '"status"[[:space:]]*:[[:space:]]*"succeeded"' "nginx-test-response-${index}.json" || { echo 'nginx -t 响应 status 不是 succeeded'; exit 1; }
                grep -Eq '"phase"[[:space:]]*:[[:space:]]*"nginx_test_succeeded"' "nginx-test-response-${index}.json" || { echo 'nginx -t 响应未完成'; exit 1; }
                printf '%s\n' nginx_test_succeeded > "release-status-${index}.txt"
                """)
              } finally {
                if (fileExists("release-status-${index}.txt") && readFile("release-status-${index}.txt").trim() != 'skipped' && fileExists("nginx-test-response-${index}.json")) {
                  showReleaseLog('nginx -t 配置检测', "${env.RELEASE_TYPE}:${env.RELEASE_SERVER_NAME}",
                    fileExists("release-node-${index}.txt") ? readFile("release-node-${index}.txt").trim() : env.RELEASE_NODE_URL,
                    fileExists("nginx-test-http-${index}.txt") ? readFile("nginx-test-http-${index}.txt").trim() : '-',
                    readFile("nginx-test-response-${index}.json"), [
                      [title: '执行 nginx -t', name: 'nginx_test']
                    ])
                }
              }
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
              try {
                sh(script: """#!/bin/sh
                set -eu
                rm -f "nginx-reload-response-${index}.json" "nginx-reload-http-${index}.txt"
                node_id="\$(cat "release-node-${index}.txt")"
                state="\$(cat "release-status-${index}.txt")"
                if [ "\$state" = skipped ]; then exit 0; fi
                test "\$state" = nginx_test_succeeded || { echo "节点 \$node_id 未完成 nginx -t：\$state"; exit 1; }
                release_id="\$(cat "release-id-${index}.txt")"
                echo "\$release_id" | grep -Eq '^[0-9a-fA-F-]{36}\$' || { echo '缺少合法 release_id'; exit 1; }
                printf '{"env":"%s","release_id":"%s"}' "\$RELEASE_ENV" "\$release_id" > "nginx-reload-request-${index}.json"
                code="\$(curl -sS -o 'nginx-reload-response-${index}.json' -w '%{http_code}' --max-time 120 \\
                  -X POST "\$RELEASE_NODE_URL/api/v1/releases/nginx/reload" \\
                  -H 'Content-Type: application/json' \\
                  -H "X-Release-Token: \$RELEASE_TOKEN" \\
                  --data-binary @'nginx-reload-request-${index}.json')"
                printf '%s\n' "\$code" > "nginx-reload-http-${index}.txt"
                test "\$code" = 200 || { echo "reload 接口应返回 HTTP 200，实际为 \$code"; exit 1; }
                grep -Eq '"status"[[:space:]]*:[[:space:]]*"succeeded"' "nginx-reload-response-${index}.json" || { echo 'reload 响应 status 不是 succeeded'; exit 1; }
                printf '%s\n' complete > "release-status-${index}.txt"
                """)
              } finally {
                if (fileExists("release-status-${index}.txt") && readFile("release-status-${index}.txt").trim() != 'skipped' && fileExists("nginx-reload-response-${index}.json")) {
                  showReleaseLog('nginx -s reload 与生效验证', "${env.RELEASE_TYPE}:${env.RELEASE_SERVER_NAME}",
                    fileExists("release-node-${index}.txt") ? readFile("release-node-${index}.txt").trim() : env.RELEASE_NODE_URL,
                    fileExists("nginx-reload-http-${index}.txt") ? readFile("nginx-reload-http-${index}.txt").trim() : '-',
                    readFile("nginx-reload-response-${index}.json"), [
                      [title: '执行 nginx -s reload', name: 'reload'],
                      [title: '验证配置生效', name: 'verify_activation']
                    ])
                }
              }
            }
          }
        }
      }
    }
  }
  post {
    success { echo 'HTTP 发布完成。' }
    failure { echo '发布失败，具体 Git、nginx -t、reload 及回滚结果见上方控制台。' }
  }
}
