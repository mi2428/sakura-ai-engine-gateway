#!/usr/bin/env bash
set -euo pipefail

tools="${RUNNER_TEMP:?}/check-tools"
mkdir -p "$tools"
cd "$tools"
curl -fsSL --retry 3 https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-linux-amd64.tar.gz -o golangci.tar.gz
echo '9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea  golangci.tar.gz' | sha256sum --check -
tar -xzf golangci.tar.gz --strip-components=1 golangci-lint-2.15.0-linux-amd64/golangci-lint
curl -fsSL --retry 3 https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-linux-amd64.tar.gz -o kubeconform.tar.gz
echo '9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883  kubeconform.tar.gz' | sha256sum --check -
tar -xzf kubeconform.tar.gz kubeconform
curl -fsSL --retry 3 https://github.com/hadolint/hadolint/releases/download/v2.15.1/hadolint-linux-x86_64 -o hadolint
echo 'c7187db94eeeeca956519a6af171adc31453941a1e777961f6e680f697c8c507  hadolint' | sha256sum --check -
chmod +x hadolint
printf '%s\n' "$tools" >> "${GITHUB_PATH:?}"
