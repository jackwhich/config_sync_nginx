// Jenkins 的 agent any 仅指定构建执行器；发布通过 HTTP 直连各节点。
// resume/rollback 从指定构建读取批次记录，需要 Jenkins Copy Artifact 插件。
pipeline {
  agent any
  options { timestamps(); disableConcurrentBuilds() }
  parameters {
    choice(name: 'ACTION', choices: ['update', 'resume', 'rollback'], description: '发布、继续原批次、恢复原批次各节点基线')
    choice(name: 'SERVER_NAME', choices: ['ybf-uat-nginx', 'jp-ybf-uat-nginx'], description: '配置中允许的站点')
    string(name: 'COMMIT_ID', defaultValue: '', description: 'update 必填：制品仓库完整提交 ID，不使用 Jenkinsfile 仓库的 GIT_COMMIT')
    string(name: 'SOURCE_BUILD', defaultValue: '', description: 'resume/rollback 必填：保存原 release-batch.json 的本 Job 构建编号')
    choice(name: 'FAILURE_POLICY', choices: ['stop', 'restore'], description: '批次开始前确定失败策略；首次部署只能 stop')
  }
  environment {
    RELEASE_ENV = 'uat'
    RELEASE_TYPE = 'config'
    RELEASE_BRANCH = 'uat'
    RELEASE_PROJECT = 'ybf'
    RELEASE_PATH_DEST = '/data/nginx-publish'
    CREDENTIAL_ID = 'release-token-uat'
    SERVICE_URLS_ybf_uat_nginx = 'http://127.0.0.1:9166,http://127.0.0.1:9167'
    SERVICE_URLS_jp_ybf_uat_nginx = 'http://127.0.0.1:9168,http://127.0.0.1:9169'
  }
  stages {
    stage('Prepare HTTP batch') {
      steps {
        script {
          env.RELEASE_BATCH_FILE = "http-release-${env.BUILD_NUMBER}/release-batch.json"
          env.RELEASE_SERVER_NAME = params.SERVER_NAME
          env.RELEASE_COMMIT = params.COMMIT_ID.trim()
          env.RELEASE_FAILURE_POLICY = params.FAILURE_POLICY
          env.RELEASE_URLS = env["SERVICE_URLS_${params.SERVER_NAME.replace('-', '_')}"]
          if (params.ACTION == 'update') {
            if (!(env.RELEASE_COMMIT ==~ /(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})/)) { error('必须填写制品仓库完整提交 ID') }
            if (!env.RELEASE_URLS?.trim()) { error('未配置节点 HTTP 地址') }
          } else {
            if (!(params.SOURCE_BUILD ==~ /[0-9]+/)) { error('SOURCE_BUILD 须为构建编号') }
            copyArtifacts(projectName: env.JOB_NAME, selector: specific(params.SOURCE_BUILD), filter: '**/release-batch.json', target: "http-release-${env.BUILD_NUMBER}", flatten: true)
          }
        }
      }
    }
    stage('Execute HTTP batch') {
      steps {
        withCredentials([string(credentialsId: env.CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          withEnv(["RELEASE_ACTION=${params.ACTION}"]) {
            sh 'bash scripts/release-apply.sh "$RELEASE_ACTION" --batch-file "$RELEASE_BATCH_FILE"'
          }
        }
      }
    }
  }
  post {
    always {
      archiveArtifacts(artifacts: "http-release-${env.BUILD_NUMBER}/release-batch.json", allowEmptyArchive: true)
    }
    success { echo 'HTTP 批次已完成；查看归档的逐节点结果。' }
    failure { echo '批次未完成；保留原批次文件，使用 resume 查询未知结果，或 rollback 恢复已确认的成功节点。' }
  }
}
