#!/usr/bin/env bash
# Prints markdown release notes for <tag>: commits since the previous v* tag, or since the
# merge-base with the upstream mirror for the first release. Usage: release-notes.sh <tag>
set -euo pipefail

tag="${1:?usage: release-notes.sh <tag>}"
repo="${GITHUB_REPOSITORY:-iv7777/3x-ui}"
base_ref="${NOTES_BASE_REF:-origin/main}"

if prev="$(git describe --tags --abbrev=0 --match 'v[0-9]*' "${tag}^" 2> /dev/null)"; then
    compare_base="${prev}"
else
    prev="$(git merge-base "${tag}" "${base_ref}")"
    compare_base="${prev}"
fi

declare -A section=([feat]="" [fix]="" [perf]="" [other]="")
pattern='^([a-z]+)(\([^)]*\))?!?: (.+)$'
while IFS=$'\t' read -r sha subject || [[ -n "${sha}" ]]; do
    [[ -z "${sha}" ]] && continue
    [[ "${subject}" == chore\(release\)* ]] && continue
    kind="other"
    text="${subject}"
    if [[ "${subject}" =~ ${pattern} ]]; then
        case "${BASH_REMATCH[1]}" in
            feat) kind="feat" ;;
            fix) kind="fix" ;;
            perf) kind="perf" ;;
        esac
        scope="${BASH_REMATCH[2]}"
        text="${BASH_REMATCH[3]}"
        [[ -n "${scope}" ]] && text="**${scope:1:-1}**: ${text}"
    fi
    section[${kind}]+="- ${text} ([\`${sha}\`](https://github.com/${repo}/commit/${sha}))"$'\n'
done < <(git log --no-merges --pretty=format:'%h%x09%s' "${compare_base}..${tag}")

print_section() {
    [[ -n "${section[$1]}" ]] && printf '### %s\n\n%s\n' "$2" "${section[$1]}"
    return 0
}

printf '## What changed\n\n'
print_section feat "Features"
print_section fix "Fixes"
print_section perf "Performance"
print_section other "Other changes"
printf '**Full changelog**: https://github.com/%s/compare/%s...%s\n\n' "${repo}" "${compare_base}" "${tag}"
printf '## Install or update\n\n```bash\nbash <(curl -Ls https://raw.githubusercontent.com/%s/my-branch/update.sh)\n```\n\n' "${repo}"
printf 'Every archive has a `.sha256` file next to it; the installer and updater verify it.\n'
