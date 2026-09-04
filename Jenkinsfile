// HTTP 发布流水线。Jenkins 的 agent 只指定构建执行器，节点部署始终使用 HTTP。
// 本 Job 发布配置/白名单：commit 取当前 Job 检出的 GitLab 仓库 GIT_COMMIT。
// 回滚只用于前端发布 Job，不在本流水线提供。
pipeline {
  agent any
  options { timestamps(); disableConcurrentBuilds() }
  parameters {
    choice(name: 'ACTION', choices: ['update'], description: '配置/白名单发布')
    choice(name: 'SERVER_NAME', choices: ['ybf-uat-nginx', 'jp-ybf-uat-nginx'], description: '配置中允许的站点')
  }
  environment {
    RELEASE_ENV = 'uat'
    RELEASE_TYPE = 'config'
    RELEASE_BRANCH = 'uat'
    RELEASE_PROJECT = 'ybf'
    RELEASE_PATH_DEST = '/data/nginx-publish'
    RELEASE_CREDENTIAL_ID = 'release-token-uat'
    RELEASE_FAILURE_POLICY = 'stop'
    SERVICE_URLS_ybf_uat_nginx = 'http://127.0.0.1:9166,http://127.0.0.1:9167'
    SERVICE_URLS_jp_ybf_uat_nginx = 'http://127.0.0.1:9168,http://127.0.0.1:9169'
  }
  stages {
    stage('Prepare HTTP batch') {
      steps {
        script {
          if (!isUnix()) { error('发布执行器需要 Bash 和 Python 3.9+，请使用 Linux 执行器') }
          env.RELEASE_ACTION = params.ACTION
          env.RELEASE_BATCH_FILE = "http-release-${env.BUILD_NUMBER}/release-batch.json"
          env.RELEASE_SERVER_NAME = params.SERVER_NAME
          env.RELEASE_URLS = env["SERVICE_URLS_${params.SERVER_NAME.replace('-', '_')}"]
          def gitBranch = env.GIT_BRANCH ?: env.BRANCH_NAME ?: env.RELEASE_BRANCH
          env.RELEASE_BRANCH = gitBranch.replaceFirst(/^refs\/heads\//, '').replaceFirst(/^origin\//, '')
          env.RELEASE_COMMIT = (env.GIT_COMMIT ?: '').trim().toLowerCase()
          if (!env.RELEASE_COMMIT) {
            env.RELEASE_COMMIT = sh(script: 'git rev-parse HEAD', returnStdout: true).trim().toLowerCase()
          }
          if (!(env.RELEASE_COMMIT ==~ /(?:[a-f0-9]{40}|[a-f0-9]{64})/)) {
            error('当前 Job 检出的 GitLab 提交必须是完整 SHA（GIT_COMMIT）')
          }
          if (!env.RELEASE_URLS?.trim()) { error('未配置该站点的节点 HTTP 地址') }
        }
      }
    }
    stage('Execute HTTP batch') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          sh 'bash scripts/release-apply.sh "$RELEASE_ACTION" --batch-file "$RELEASE_BATCH_FILE"'
        }
      }
    }
  }
  post {
    always {
      archiveArtifacts(artifacts: "http-release-${env.BUILD_NUMBER}/release-batch.json", allowEmptyArchive: true)
    }
    success { echo 'HTTP 批次已完成；查看归档的逐节点结果。' }
    failure { echo '发布失败，具体检查/reload 错误见上方控制台和归档结果。配置/白名单 Job 不提供 rollback；修复提交后重新 update。' }
  }
}
