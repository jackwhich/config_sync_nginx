#!/usr/bin/env bash
# Linux CI: run npm ci && npm run build before invoking this script.
set -euo pipefail
set +x
umask 077
if [[ ${1:-} != push || $# != 3 ]]; then
  echo 'usage: bash scripts/frontend-artifact.sh push <dist-directory> <new-output-directory>' >&2
  exit 2
fi
: "${HARBOR_REPOSITORY:?registry/project/app-dist required}"
: "${RELEASE_COMMIT:?full Git SHA required}"
: "${HARBOR_USERNAME:?CI robot username required}"
: "${HARBOR_PASSWORD:?CI robot password required}"
: "${HTTPS_PROXY:?CI HTTPS egress proxy required}"
[[ $RELEASE_COMMIT =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] || { echo 'full lowercase Git SHA required' >&2; exit 2; }
[[ $HARBOR_REPOSITORY =~ ^[a-z0-9][a-z0-9.:-]*/[a-z0-9][a-z0-9._/-]*$ && $HARBOR_REPOSITORY != *..* ]] || { echo 'invalid Harbor repository' >&2; exit 2; }
[[ -s $2/index.html && ! -L $2/index.html ]] || { echo 'built dist/index.html required' >&2; exit 2; }
artifact_dist=$(cd -- "$2" && pwd)
# Reusing a build directory must not silently replace an earlier artifact record.
mkdir -- "$3"
artifact_output=$(cd -- "$3" && pwd)
case "$artifact_output/" in "$artifact_dist/"*) echo 'output must be outside dist' >&2; exit 2;; esac
# Finish packaging before login/push. Archive root is index.html, not dist/index.html.
tar -czf "$artifact_output/dist.tar.gz" -C "$artifact_dist" .
cd -- "$artifact_output"
sha256sum dist.tar.gz > dist.tar.gz.sha256
artifact_auth=$(mktemp -d)
trap 'rm -rf -- "$artifact_auth"' EXIT
oras_bin=${ORAS_BIN:-oras}
registry=${HARBOR_REPOSITORY%%/*}
printf '%s' "$HARBOR_PASSWORD" | "$oras_bin" login "$registry" -u "$HARBOR_USERNAME" --password-stdin --registry-config "$artifact_auth/auth.json"
"$oras_bin" push "$HARBOR_REPOSITORY:$RELEASE_COMMIT" \
  --registry-config "$artifact_auth/auth.json" \
  --artifact-type application/vnd.nginx.frontend.dist.v1 \
  --annotation "org.opencontainers.image.revision=$RELEASE_COMMIT" \
  --export-manifest oci-manifest.json \
  dist.tar.gz:application/gzip dist.tar.gz.sha256:text/plain
# Hash the manifest actually pushed, rather than resolving a mutable tag afterward.
read -r artifact_hash _ < <(sha256sum oci-manifest.json)
printf 'sha256:%s\n' "$artifact_hash" > artifact.digest
printf '%s@sha256:%s\n' "$HARBOR_REPOSITORY" "$artifact_hash" > artifact.ref
printf 'Artifact ready: %s@sha256:%s\n' "$HARBOR_REPOSITORY" "$artifact_hash"
# Intentionally no prod tag here. Update it only after all HTTP nodes are verified.
