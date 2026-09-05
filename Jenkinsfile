// HTTP 发布流水线。Jenkins 的 agent 只指定构建执行器，节点部署用 curl 直连 HTTP。
// 本 Job 只发布 config / whitelist；前端制品请使用独立的 Jenkinsfile。
// 发布提交来自下方配置的 GitLab 仓库，而不是 Jenkinsfile 所在仓库。
def parseReleaseResponse(text, label, statusCode) {
  try {
    return new groovy.json.JsonSlurperClassic().parseText(text)
  } catch (Exception ignored) {
    echo "${label}响应不是有效 JSON：\n${text}"
    error("${label}返回 HTTP ${statusCode}，无法解析响应")
  }
}

def showReleaseStage(title, node, result, statusCode, stepNames, releaseEnv, releaseType, serverName, commit, branch) {
  def steps = result.steps ?: []
  echo "========== 节点发布详情：${title} =========="
  echo "节点主机：${node.node_id ?: 'unknown'}  服务地址：${node.url}"
  echo "发布对象：type=${result.type ?: releaseType} server_name=${result.server_name ?: serverName} commit=${result.commit_id ?: commit}"
  echo "release_id=${result.release_id ?: node.release_id ?: '-'}"
  stepNames.each { spec ->
    def step = steps.find { item -> item.name == spec.name }
    if (step == null) {
      echo "${spec.title}: 未执行"
    } else {
      echo "${spec.title}: status=${step.status ?: 'unknown'} duration_ms=${step.duration_ms ?: 0}"
      if (step.message) {
        echo "  ${step.message}"
      }
    }
  }
  echo "节点状态：${node.node_id ?: 'unknown'} status=${result.status ?: '-'} activation=${result.activation_status ?: '-'} HTTP ${statusCode}"
  echo '=========================================='
}

def failReleaseStage(title, text, statusCode) {
  echo "${title}失败，HTTP ${statusCode}：\n${groovy.json.JsonOutput.prettyPrint(text)}"
  error("节点发布失败：${title}，HTTP ${statusCode}")
}

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
            def progress = [nodes: []]
            def saveProgress = { writeFile file: 'release-progress.json', text: groovy.json.JsonOutput.toJson(progress) }
            urls.eachWithIndex { url, index ->
              env.RELEASE_NODE_URL = url.replaceAll(/\/+$/, '')
              echo "同步 ${env.RELEASE_NODE_URL}  type=${env.RELEASE_TYPE} server_name=${env.RELEASE_SERVER_NAME} commit=${env.RELEASE_COMMIT}"
              def code = sh(script: '''
                set -eu
                node="$RELEASE_NODE_URL"
                echo "GET $node/healthz"
                health="$(curl -sS --max-time 30 "$node/healthz")"
                printf '%s' "$health" > "sync-health-''' + index + '''.json"
                echo "$health" | grep -Eq '"release_contract"[[:space:]]*:[[:space:]]*2' || { echo 'release_contract 必须为 2'; exit 1; }
                echo "$health" | grep -Eq '"publish_ready"[[:space:]]*:[[:space:]]*true' || { echo '节点 publish_ready 不为 true'; exit 1; }
                echo "POST $node/api/v1/releases/apply"
                code="$(curl -sS -o "sync-response-''' + index + '''.json" -w '%{http_code}' --max-time 420 \
                  -X POST "$node/api/v1/releases/apply" \
                  -H 'Content-Type: application/json' \
                  -H "X-Release-Token: $RELEASE_TOKEN" \
                  --data-binary @release-request.json)"
                printf '%s' "$code"
              ''', returnStdout: true).trim()

              def responseText = readFile("sync-response-${index}.json")
              def response = parseReleaseResponse(responseText, '同步发布', code)
              def health = parseReleaseResponse(readFile("sync-health-${index}.json"), '健康检查', '200')
              def node = [url: env.RELEASE_NODE_URL, node_id: health.node_id, release_id: response.release_id,
                          skipped: code == '200' && response.status == 'skipped', sync: response, sync_http_code: code]
              showReleaseStage('同步 Git 并切换 latest', node, response, code, [
                [title: "Git 更新（branch=${env.RELEASE_BRANCH}）", name: 'fetch'],
                [title: '配置快照准备', name: 'prepare_snapshot'],
                [title: '切换 latest', name: 'switch']
              ], env.RELEASE_ENV, env.RELEASE_TYPE, env.RELEASE_SERVER_NAME, env.RELEASE_COMMIT, env.RELEASE_BRANCH)
              if (node.skipped) {
                echo "${node.node_id ?: node.url} 已是目标 commit，后续 nginx -t 和 reload 阶段将跳过。"
              } else {
                if (code != '202' || response.status != 'running' || response.phase != 'awaiting_nginx_test') {
                  failReleaseStage('同步 Git 并切换 latest', responseText, code)
                }
              }
              progress.nodes << node
              saveProgress()
            }
          }
        }
      }
    }
    stage('nginx -t 配置检测') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          script {
            def progress = parseReleaseResponse(readFile('release-progress.json'), '发布进度文件', 'local')
            def nodes = progress.nodes ?: []
            if (!nodes) { error('同步阶段没有可继续的节点') }
            def saveProgress = { writeFile file: 'release-progress.json', text: groovy.json.JsonOutput.toJson(progress) }
            nodes.eachWithIndex { node, index ->
              if (node.skipped) {
                echo "${node.node_id ?: node.url} 已是目标 commit，跳过 nginx -t。"
                return
              }
              env.RELEASE_NODE_URL = node.url
              writeFile file: "nginx-test-request-${index}.json", text: groovy.json.JsonOutput.toJson([env: env.RELEASE_ENV, release_id: node.release_id])
              def code = sh(script: """
                set -eu
                echo "POST \$RELEASE_NODE_URL/api/v1/releases/nginx/test"
                code=\"\$(curl -sS -o 'nginx-test-response-${index}.json' -w '%{http_code}' --max-time 120 \\
                  -X POST \"\$RELEASE_NODE_URL/api/v1/releases/nginx/test\" \\
                  -H 'Content-Type: application/json' \\
                  -H \"X-Release-Token: \$RELEASE_TOKEN\" \\
                  --data-binary @'nginx-test-request-${index}.json')\"
                printf '%s' \"\$code\"
              """, returnStdout: true).trim()
              def responseText = readFile("nginx-test-response-${index}.json")
              def response = parseReleaseResponse(responseText, 'nginx -t', code)
              node.nginx_test = response
              node.nginx_test_http_code = code
              saveProgress()
              showReleaseStage('nginx -t 配置检测', node, response, code, [
                [title: 'Nginx 配置检测（nginx -t）', name: 'nginx_test']
              ], env.RELEASE_ENV, env.RELEASE_TYPE, env.RELEASE_SERVER_NAME, env.RELEASE_COMMIT, env.RELEASE_BRANCH)
              if (code != '202' || response.status != 'running' || response.phase != 'awaiting_nginx_reload') {
                failReleaseStage('nginx -t 配置检测', responseText, code)
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
            def progress = parseReleaseResponse(readFile('release-progress.json'), '发布进度文件', 'local')
            def nodes = progress.nodes ?: []
            if (!nodes) { error('同步阶段没有可继续的节点') }
            def saveProgress = { writeFile file: 'release-progress.json', text: groovy.json.JsonOutput.toJson(progress) }
            nodes.eachWithIndex { node, index ->
              if (node.skipped) {
                echo "${node.node_id ?: node.url} 已是目标 commit，跳过 nginx -s reload。"
                return
              }
              env.RELEASE_NODE_URL = node.url
              writeFile file: "nginx-reload-request-${index}.json", text: groovy.json.JsonOutput.toJson([env: env.RELEASE_ENV, release_id: node.release_id])
              def code = sh(script: """
                set -eu
                echo "POST \$RELEASE_NODE_URL/api/v1/releases/nginx/reload"
                code=\"\$(curl -sS -o 'nginx-reload-response-${index}.json' -w '%{http_code}' --max-time 120 \\
                  -X POST \"\$RELEASE_NODE_URL/api/v1/releases/nginx/reload\" \\
                  -H 'Content-Type: application/json' \\
                  -H \"X-Release-Token: \$RELEASE_TOKEN\" \\
                  --data-binary @'nginx-reload-request-${index}.json')\"
                printf '%s' \"\$code\"
              """, returnStdout: true).trim()
              def responseText = readFile("nginx-reload-response-${index}.json")
              def response = parseReleaseResponse(responseText, 'nginx -s reload', code)
              node.nginx_reload = response
              node.nginx_reload_http_code = code
              saveProgress()
              showReleaseStage('nginx -s reload 与生效验证', node, response, code, [
                [title: 'Nginx 重载（nginx -s reload）', name: 'reload'],
                [title: '生效验证', name: 'verify_activation']
              ], env.RELEASE_ENV, env.RELEASE_TYPE, env.RELEASE_SERVER_NAME, env.RELEASE_COMMIT, env.RELEASE_BRANCH)
              if (code != '200' || response.status != 'succeeded') {
                failReleaseStage('nginx -s reload 与生效验证', responseText, code)
              }
            }
          }
        }
      }
    }
  }
  post {
    success { echo 'HTTP 发布完成。' }
    failure { echo '发布失败，具体 Git、nginx -t、reload 及自动恢复结果见上方控制台。若节点进入 recovery_required，请先恢复该节点再重新发布。' }
  }
}
