// HTTP 发布流水线。Jenkins 的 agent 只指定构建执行器，节点部署始终使用 HTTP。
// 前端制品由应用 CI 预先构建并推送 Harbor；本流水线调用节点服务 pull。
// resume/rollback 通过 Copy Artifact 插件读取本 Job 原构建的批次记录。
pipeline {
  agent any
  options { timestamps(); disableConcurrentBuilds() }
  parameters {
    choice(name: 'ACTION', choices: ['update', 'resume', 'rollback'], description: '发布、继续原批次、恢复原批次各节点基线')
    string(name: 'DEPLOY_ENV', defaultValue: 'uat', description: '发布环境；resume/rollback 必须与原批次一致')
    choice(name: 'RELEASE_TYPE', choices: ['config', 'whitelist', 'frontend_static'], description: 'update 使用；节点 targets 必须启用此类型')
    string(name: 'BRANCH', defaultValue: '', description: 'Git 发布分支，作为 POST 顶层 branch 传入；可省略，前端不使用')
    string(name: 'COMMIT_ID', defaultValue: '', description: 'update 必填：制品对应的完整 40/64 位 SHA，不使用本仓库的 GIT_COMMIT')
    string(name: 'SERVER_NAME', defaultValue: '', description: 'update 必填：站点目录键，作为 params.server_name 传入')
    string(name: 'PATH_DEST', defaultValue: '', description: 'update 必填：节点上的绝对部署根目录，作为 params.path_dest 传入')
    text(name: 'SERVICE_URLS', defaultValue: '', description: 'update 必填：节点 HTTP 地址，用逗号、空格或换行分隔；按顺序发布')
    string(name: 'PROJECT', defaultValue: '', description: '可选项目标识，不决定发布目录')
    string(name: 'ARTIFACT_DIGEST', defaultValue: '', description: '前端可选 sha256:摘要；留空由首台节点按完整 SHA 解析并固定后续节点摘要')
    string(name: 'TOKEN_CREDENTIAL_ID', defaultValue: '', description: '必填：Jenkins Secret text 凭据 ID，内容为对应环境的发布 Token')
    string(name: 'SOURCE_BUILD', defaultValue: '', description: 'resume/rollback 必填：保存原 release-batch.json 的本 Job 构建编号')
    choice(name: 'FAILURE_POLICY', choices: ['stop', 'restore'], description: 'update 使用；首个失败即停止后续发布，restore 会恢复已成功节点；首次部署只能 stop')
  }
  stages {
    stage('Prepare HTTP batch') {
      steps {
        script {
          if (!isUnix()) { error('发布执行器需要 Bash 和 Python 3.9+，请使用 Linux 执行器') }
          env.RELEASE_BATCH_FILE = "http-release-${env.BUILD_NUMBER}/release-batch.json"
          env.RELEASE_ACTION = params.ACTION
          env.RELEASE_ENV = params.DEPLOY_ENV.trim()
          env.RELEASE_CREDENTIAL_ID = params.TOKEN_CREDENTIAL_ID.trim()
          if (!env.RELEASE_ENV) { error('DEPLOY_ENV 不能为空') }
          if (!env.RELEASE_CREDENTIAL_ID) { error('TOKEN_CREDENTIAL_ID 不能为空，请填写对应环境的 Secret text 凭据 ID') }
          if (params.ACTION == 'update') {
            env.RELEASE_TYPE = params.RELEASE_TYPE
            env.RELEASE_BRANCH = params.RELEASE_TYPE == 'frontend_static' ? '' : params.BRANCH.trim()
            env.RELEASE_COMMIT = params.COMMIT_ID.trim().toLowerCase()
            env.RELEASE_SERVER_NAME = params.SERVER_NAME.trim()
            env.RELEASE_PATH_DEST = params.PATH_DEST.trim()
            env.RELEASE_PROJECT = params.PROJECT.trim()
            env.RELEASE_URLS = params.SERVICE_URLS.trim()
            env.RELEASE_ARTIFACT_DIGEST = params.ARTIFACT_DIGEST.trim()
            env.RELEASE_FAILURE_POLICY = params.FAILURE_POLICY
            if (!(env.RELEASE_COMMIT ==~ /(?:[a-f0-9]{40}|[a-f0-9]{64})/)) { error('必须填写制品对应的完整提交 ID') }
            if (!env.RELEASE_SERVER_NAME || !env.RELEASE_PATH_DEST.startsWith('/')) { error('必须填写 SERVER_NAME 和绝对 PATH_DEST') }
            if (!env.RELEASE_URLS) { error('SERVICE_URLS 不能为空') }
          } else {
            def sourceBuild = params.SOURCE_BUILD.trim()
            if (!(sourceBuild ==~ /[1-9][0-9]*/)) { error('SOURCE_BUILD 须为构建编号') }
            // 原批次中的环境、类型、分支、目录、摘要和 UUID 是权威参数。
            copyArtifacts(projectName: env.JOB_NAME, selector: specific(sourceBuild), filter: "http-release-${sourceBuild}/release-batch.json", target: "http-release-${env.BUILD_NUMBER}", flatten: true)
          }
        }
      }
    }
    stage('Execute HTTP batch') {
      steps {
        withCredentials([string(credentialsId: env.RELEASE_CREDENTIAL_ID, variable: 'RELEASE_TOKEN')]) {
          // 参数通过环境变量传递，Token 不写入 JSON；非零退出直接使流水线失败。
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
    failure { echo '发布失败，具体检查/reload 错误见上方控制台和归档结果；使用原批次 resume 查询未知结果，或 rollback 恢复已确认成功的节点。' }
  }
}
