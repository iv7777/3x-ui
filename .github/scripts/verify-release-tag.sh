#!/usr/bin/env bash
# Refuses a release tag that disagrees with internal/config/version or that was
# cut from a commit outside the release branch. Usage: verify-release-tag.sh <tag> [branch]
set -euo pipefail

tag="${1:?usage: verify-release-tag.sh <tag> [branch]}"
branch="${2:-my-branch}"
version="$(tr -d '[:space:]' < internal/config/version)"

if [[ "${tag}" != "v${version}" ]]; then
    echo "::error::tag ${tag} does not match internal/config/version (${version}); the two must agree" >&2
    exit 1
fi

if ! git merge-base --is-ancestor "${GITHUB_SHA:-HEAD}" "origin/${branch}"; then
    echo "::error::tag ${tag} points at a commit that is not on ${branch}; merge it into ${branch} first" >&2
    exit 1
fi
echo "tag ${tag} matches version ${version} and is on ${branch}"
