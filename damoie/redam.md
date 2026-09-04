cd /data/jenkins/dcops/nginx_whitelist
ansible-playbook -i inventory/prod-nginx-conf/ playbook/rsync-conf-nginx.yaml -e "{
    'SERVER_NAME': '${SERVER_NAME}',
    'DEPLOY_HOSTS': '${SERVER_NAME}',
    'check_commitID': '${GIT_COMMIT}',
    'path_src': '${WORKSPACE}/${SERVER_NAME}',
    'type': 'config',
    'path_dest': '/data/nginx-conf'
    }" --tags $action_type