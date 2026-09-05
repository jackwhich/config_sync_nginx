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
    stage('Publish') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          script {
            def urls = env.RELEASE_URLS.split(/[\s,]+/).findAll { it }
            if (!urls) { error('SERVICE_URLS 不能为空') }
            urls.each { url ->
              env.RELEASE_NODE_URL = url.replaceAll(/\/+$/, '')
              echo "发布 ${env.RELEASE_NODE_URL}  type=${env.RELEASE_TYPE} server_name=${env.RELEASE_SERVER_NAME} commit=${env.RELEASE_COMMIT}"
              def code = sh(script: '''
                set -eu
                node="$RELEASE_NODE_URL"
                echo "GET $node/healthz"
                health="$(curl -sS --max-time 30 "$node/healthz")"
                printf '%s' "$health" > health-response.json
                echo "$health" | grep -Eq '"release_contract"[[:space:]]*:[[:space:]]*2' || { echo 'release_contract 必须为 2'; exit 1; }
                echo "$health" | grep -Eq '"publish_ready"[[:space:]]*:[[:space:]]*true' || { echo '节点 publish_ready 不为 true'; exit 1; }
                echo "POST $node/api/v1/releases/apply"
                code="$(curl -sS -o apply-response.json -w '%{http_code}' --max-time 420 \
                  -X POST "$node/api/v1/releases/apply" \
                  -H 'Content-Type: application/json' \
                  -H "X-Release-Token: $RELEASE_TOKEN" \
                  --data-binary @release-request.json)"
                printf '%s' "$code"
              ''', returnStdout: true).trim()

              def responseText = readFile('apply-response.json')
              def response
              try {
                response = new groovy.json.JsonSlurperClassic().parseText(responseText)
              } catch (Exception ignored) {
                echo "发布响应不是有效 JSON：\n${responseText}"
                error("HTTP ${code}，无法解析节点发布响应")
              }

              def healthText = readFile('health-response.json')
              def health
              try {
                health = new groovy.json.JsonSlurperClassic().parseText(healthText)
              } catch (Exception ignored) {
                echo "健康检查响应不是有效 JSON：\n${healthText}"
                error('无法识别发布节点主机名')
              }

              def steps = response.steps ?: []
              def showStep = { title, name ->
                def step = steps.find { item -> item.name == name }
                if (step == null) {
                  echo "${title}: 未执行"
                } else {
                  echo "${title}: status=${step.status ?: 'unknown'} duration_ms=${step.duration_ms ?: 0}"
                  if (step.message) {
                    echo "  ${step.message}"
                  }
                }
              }

              echo '========== 节点发布详情 =========='
              echo "节点主机：${health.node_id ?: 'unknown'}  服务地址：${env.RELEASE_NODE_URL}"
              echo "发布对象：type=${response.type ?: env.RELEASE_TYPE} server_name=${response.server_name ?: env.RELEASE_SERVER_NAME} commit=${response.commit_id ?: env.RELEASE_COMMIT}"
              echo "release_id=${response.release_id ?: '-'}"
              showStep("Git 更新（branch=${env.RELEASE_BRANCH}）", 'fetch')
              showStep('配置快照准备', 'prepare_snapshot')
              showStep('切换 latest', 'switch')
              showStep('Nginx 配置检测（nginx -t）', 'nginx_test')
              showStep('Nginx 重载（nginx -s reload）', 'reload')
              showStep('生效验证', 'verify_activation')
              echo "节点完成：${health.node_id ?: 'unknown'} status=${response.status ?: '-'} activation=${response.activation_status ?: '-'} HTTP ${code}"
              echo '=================================='
              if (code != '200') {
                echo "完整发布响应：\n${groovy.json.JsonOutput.prettyPrint(responseText)}"
                error("节点发布失败，HTTP ${code}")
              }
            }
          }
        }
      }
    }
  }
  post {
    success { echo 'HTTP 发布完成。' }
    failure { echo '发布失败，具体检查/reload 错误见上方控制台。配置/白名单 Job 不提供 rollback；修复提交后重新 update。' }
  }
}
