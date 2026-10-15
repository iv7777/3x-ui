#!/usr/bin/env bash
#
# update-rollback-test.sh — exercises update.sh's backup, rollback, pruning and health
# check, and x-ui.sh's rollback command, against a throwaway tree with a stubbed systemctl. Needs root, like update.sh.
# Usage: bash deploy/test/update-rollback-test.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if [[ $EUID -ne 0 ]]; then
    echo "SKIP: update.sh refuses to load without root"
    exit 0
fi

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/stubs"
cat > "$T/stubs/systemctl" << 'STUB'
#!/usr/bin/env bash
echo "$*" >> "${STUB_LOG}"
if [[ "$1" == "is-active" ]]; then [[ "${STUB_ACTIVE:-1}" == "1" ]]; else exit 0; fi
STUB
cat > "$T/stubs/curl" << 'STUB'
#!/usr/bin/env bash
# Serves update.sh's downloads from the test tree: the release archive, x-ui.sh, and a 404 for the checksum.
out="" want_code="" url=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o | -fLRo) out="$2"; shift ;;
        -w) want_code=1; shift ;;
        http*) url="$1" ;;
    esac
    shift
done
case "$url" in
    *.sha256) [[ -n "$want_code" ]] && printf 404; exit 0 ;;
    *.tar.gz)
        [[ -s "${STUB_TARBALL:-}" ]] || exit 22
        cp "$STUB_TARBALL" "$out" ;;
    *x-ui.sh) echo "new-management-script" > "$out" ;;
    *) [[ -n "$out" && "$out" != /dev/null ]] && echo stub > "$out" ;;
esac
[[ -n "$want_code" ]] && printf 200
exit 0
STUB
chmod +x "$T/stubs/systemctl" "$T/stubs/curl"

export PATH="$T/stubs:$PATH" STUB_LOG="$T/systemctl.log"
export XUI_MAIN_FOLDER="$T/usr/local/x-ui" XUI_SERVICE="$T/etc/systemd/system"
export XUI_DB_FOLDER="$T/etc/x-ui" XUI_UPDATE_STATUS_FILE="$T/status.json"
export XUI_UPDATE_LIB_ONLY=1 XUI_HEALTH_WAIT=1 XUI_BACKUP_KEEP=2 XUI_BIN_LINK="$T/usr/bin/x-ui" XUI_UPDATE_TAG=v9.9.9

failure_log="$T/failures"
: > "$failure_log"
expect() { # expect <description> <expected> <actual>; cases run in subshells, so failures go to a file
    if [[ "$2" == "$3" ]]; then
        echo "  ok   $1"
    else
        echo "  FAIL $1: expected '$2', got '$3'"
        echo "$1" >> "$failure_log"
    fi
}

make_install() { # make_install <version tag>
    rm -rf "$XUI_MAIN_FOLDER" "$XUI_SERVICE"
    rm -f "$XUI_DB_FOLDER"/x-ui.db*  # the backups live under the database folder and must survive an "update"
    mkdir -p "$XUI_MAIN_FOLDER/bin" "$XUI_SERVICE" "$XUI_DB_FOLDER"
    echo "binary-$1" > "$XUI_MAIN_FOLDER/x-ui"
    echo "script-$1" > "$XUI_MAIN_FOLDER/x-ui.sh"
    echo "xray-$1" > "$XUI_MAIN_FOLDER/bin/xray-linux-amd64"
    echo "geo" > "$XUI_MAIN_FOLDER/bin/geoip.dat"
    echo "unit-$1" > "$XUI_SERVICE/x-ui.service"
    echo "db-$1" > "$XUI_DB_FOLDER/x-ui.db"
    echo "wal-$1" > "$XUI_DB_FOLDER/x-ui.db-wal"
    : > "$STUB_LOG"
}

run_case() { # run_case <name> <function>; the body runs in a subshell so exit/_fail stay contained
    echo "== $1"
    ( # shellcheck disable=SC1091
        source "$REPO_ROOT/update.sh" > /dev/null 2>&1
        current_xui_version="1.0.0"
        rm -rf "$(xui_backup_root)"
        "$2"
    ) || echo "$1: case aborted" >> "$failure_log"
}

case_round_trip() {
    make_install old
    backup_current_install > /dev/null
    expect "a backup directory was recorded" "yes" "$([[ -d "$xui_backup_current" ]] && echo yes || echo no)"
    make_install new
    echo "stray" > "$XUI_DB_FOLDER/x-ui.db-shm"
    : > "$STUB_LOG"
    bash "$xui_backup_current/restore.sh" > /dev/null
    expect "binary is back" "binary-old" "$(cat "$XUI_MAIN_FOLDER/x-ui")"
    expect "xray is back" "xray-old" "$(cat "$XUI_MAIN_FOLDER/bin/xray-linux-amd64")"
    expect "unit file is back" "unit-old" "$(cat "$XUI_SERVICE/x-ui.service")"
    expect "database is back" "db-old" "$(cat "$XUI_DB_FOLDER/x-ui.db")"
    expect "wal is back" "wal-old" "$(cat "$XUI_DB_FOLDER/x-ui.db-wal")"
    expect "a newer shm file is not left to confuse the old database" "no" "$([[ -e "$XUI_DB_FOLDER/x-ui.db-shm" ]] && echo yes || echo no)"
    expect "service was stopped then started" "yes" "$(grep -q '^stop x-ui' "$STUB_LOG" && grep -q '^start x-ui' "$STUB_LOG" && echo yes || echo no)"
}

case_postgres_database_untouched() {
    export XUI_DB_TYPE=postgres
    make_install old
    backup_current_install > /dev/null
    expect "no database copy is taken" "no" "$([[ -e "$xui_backup_current/x-ui.db" ]] && echo yes || echo no)"
    make_install new
    bash "$xui_backup_current/restore.sh" > /dev/null
    expect "program files are restored" "binary-old" "$(cat "$XUI_MAIN_FOLDER/x-ui")"
    expect "the database file is left as it was" "db-new" "$(cat "$XUI_DB_FOLDER/x-ui.db")"
}

case_prune_keeps_newest() {
    make_install old
    root="$(xui_backup_root)"
    mkdir -p "$root/20260101-000001-1.0.0" "$root/20260101-000002-1.0.0" "$root/20260101-000003-1.0.0" "$root/20260101-000004-1.0.0" "$root/notes"
    prune_backups
    expect "only the two newest backups remain" "20260101-000003-1.0.0 20260101-000004-1.0.0 notes" "$(ls -1 "$root" | tr '\n' ' ' | sed 's/ $//')"
}

case_failed_update_rolls_back() {
    make_install old
    backup_current_install > /dev/null
    make_install new
    out="$(_fail_with_rollback "ERROR: Failed to extract" 2>&1)"
    code=$?
    expect "exit code is non-zero" "2" "$code"
    expect "binary is back" "binary-old" "$(cat "$XUI_MAIN_FOLDER/x-ui")"
    expect "message says it was restored" "yes" "$(grep -q 'previous version 1.0.0 was restored' <<< "$out" && echo yes || echo no)"
}

case_no_backup_just_fails() {
    make_install new
    xui_backup_current=""
    out="$(_fail_with_rollback "ERROR: Failed to extract" 2>&1)"
    code=$?
    expect "exit code is non-zero" "2" "$code"
    expect "files are left alone" "binary-new" "$(cat "$XUI_MAIN_FOLDER/x-ui")"
}

case_unusable_backup_folder_only_warns() {
    make_install old
    : > "$T/not-a-dir"
    export XUI_BACKUP_FOLDER="$T/not-a-dir/backups"
    backup_current_install > /dev/null 2>&1
    expect "the update may go on" "0" "$?"
    expect "no backup is recorded" "" "${xui_backup_current}"
}

case_health_check() {
    make_install old
    STUB_ACTIVE=1 _xui_service_healthy
    expect "a running service is healthy" "0" "$?"
    STUB_ACTIVE=0 _xui_service_healthy
    expect "a service that never starts is not" "1" "$?"
}

# fake_binary <version> <migrate exit code>: stands in for the x-ui binary of a release.
fake_binary() {
    cat << EOF
#!/bin/sh
case "\$1" in
    -v) echo "$1" ;;
    migrate) exit $2 ;;
esac
exit 0
EOF
}

# build_release <migrate exit code>: a release archive for this machine's architecture, like release.yml packs.
build_release() {
    local root="$T/release/x-ui"
    rm -rf "$T/release" && mkdir -p "$root/bin"
    fake_binary 9.9.9 "$1" > "$root/x-ui"
    echo "xray-new" > "$root/bin/xray-linux-$(arch)"
    echo "unit-new" > "$root/x-ui.service"
    chmod +x "$root/x-ui" "$root/bin/xray-linux-$(arch)"
    tar -czf "$T/release.tar.gz" -C "$T/release" x-ui
    export STUB_TARBALL="$T/release.tar.gz"
}

# make_running_install: like make_install, but with a binary that reports its version.
make_running_install() {
    make_install old
    fake_binary 1.0.0 0 > "$XUI_MAIN_FOLDER/x-ui"
    chmod +x "$XUI_MAIN_FOLDER/x-ui"
    mkdir -p "$T/usr/bin" "$XUI_SERVICE"
    echo "old-management-script" > "$XUI_BIN_LINK"
}

run_update() { # update_x-ui exits through _fail, so run it here and report its status
    config_after_update() { :; }
    setup_fail2ban() { :; }
    ( update_x-ui ) > "$T/update.out" 2>&1
}

case_update_succeeds() {
    make_running_install
    build_release 0
    run_update
    expect "update finishes" "0" "$?"
    expect "new binary is installed" "9.9.9" "$("$XUI_MAIN_FOLDER/x-ui" -v)"
    expect "database is untouched" "db-old" "$(cat "$XUI_DB_FOLDER/x-ui.db")"
    expect "one backup of the old version is kept" "1" "$(find "$(xui_backup_root)" -mindepth 2 -name install.tar.gz | wc -l)"
}

case_migration_failure_rolls_back() {
    make_running_install
    build_release 1
    run_update
    expect "update fails" "2" "$?"
    expect "old binary is back" "1.0.0" "$("$XUI_MAIN_FOLDER/x-ui" -v)"
    expect "old management script is back" "old-management-script" "$(cat "$XUI_BIN_LINK")"
    expect "database is back" "db-old" "$(cat "$XUI_DB_FOLDER/x-ui.db")"
    expect "the failure says why" "yes" "$(grep -q 'could not migrate the database' "$T/update.out" && echo yes || echo no)"
}

case_unhealthy_service_rolls_back() {
    make_running_install
    build_release 0
    STUB_ACTIVE=0 run_update
    expect "update fails" "2" "$?"
    expect "old binary is back" "1.0.0" "$("$XUI_MAIN_FOLDER/x-ui" -v)"
    expect "the failure says why" "yes" "$(grep -q 'did not stay running' "$T/update.out" && echo yes || echo no)"
}

case_broken_archive_rolls_back() {
    make_running_install
    build_release 0
    echo "not a tarball" > "$STUB_TARBALL"
    run_update
    expect "update fails" "2" "$?"
    expect "old binary is back" "1.0.0" "$("$XUI_MAIN_FOLDER/x-ui" -v)"
    expect "old xray is back" "xray-old" "$(cat "$XUI_MAIN_FOLDER/bin/xray-linux-$(arch)")"
}

# Loads rollback() and its helpers from x-ui.sh without running the management script.
load_menu_rollback() {
    # shellcheck disable=SC1090
    source <(sed -n '/^xui_env_value() {/,/^legacy_version() {/p' "$REPO_ROOT/x-ui.sh" | sed '$d')
    LOGE() { echo "ERR: $*"; }
    LOGI() { echo "INFO: $*"; }
    before_show_menu() { :; }
    confirm() { local answer; read -r answer; [[ "$answer" == "y" ]]; }
    xui_env_file_path() { echo "$T/x-ui.env"; }
    printf "XUI_DB_FOLDER='%s'\n" "$XUI_DB_FOLDER" > "$T/x-ui.env"
    unset XUI_BACKUP_FOLDER
}

case_menu_rollback_restores_a_backup() {
    make_running_install
    backup_current_install > /dev/null
    make_install new
    load_menu_rollback
    out="$(printf '1\ny\n' | rollback 0 2>&1)"
    expect "the backup update.sh wrote is found and restored" "1.0.0" "$("$XUI_MAIN_FOLDER/x-ui" -v)"
    expect "its database came back too" "db-old" "$(cat "$XUI_DB_FOLDER/x-ui.db")"
}

case_menu_rollback_cancel_changes_nothing() {
    make_running_install
    backup_current_install > /dev/null
    make_install new
    load_menu_rollback
    printf '1\nn\n' | rollback 0 > /dev/null 2>&1
    expect "declining the confirmation keeps the new files" "binary-new" "$(cat "$XUI_MAIN_FOLDER/x-ui")"
    printf '0\n' | rollback 0 > /dev/null 2>&1
    expect "choosing 0 keeps the new files" "binary-new" "$(cat "$XUI_MAIN_FOLDER/x-ui")"
}

case_menu_rollback_without_backups() {
    make_running_install
    load_menu_rollback
    out="$(rollback 0 2>&1)"
    expect "it says there is nothing to restore" "yes" "$(grep -q 'No update backups found' <<< "$out" && echo yes || echo no)"
}

run_case "backup then restore puts everything back" case_round_trip
run_case "PostgreSQL deployments keep their database" case_postgres_database_untouched
run_case "pruning keeps the newest backups" case_prune_keeps_newest
run_case "a failed update restores the previous version" case_failed_update_rolls_back
run_case "without a backup a failure is reported as before" case_no_backup_just_fails
run_case "a backup that cannot be taken only warns" case_unusable_backup_folder_only_warns
run_case "service health check" case_health_check
run_case "a good update installs the new version and keeps a backup" case_update_succeeds
run_case "a failing migration restores the previous version" case_migration_failure_rolls_back
run_case "a service that does not stay up restores the previous version" case_unhealthy_service_rolls_back
run_case "a broken archive restores the previous version" case_broken_archive_rolls_back
run_case "x-ui rollback restores a backup update.sh wrote" case_menu_rollback_restores_a_backup
run_case "x-ui rollback can be declined" case_menu_rollback_cancel_changes_nothing
run_case "x-ui rollback with no backups says so" case_menu_rollback_without_backups

if [[ -s "$failure_log" ]]; then
    echo "$(wc -l < "$failure_log") check(s) failed"
    exit 1
fi
echo "all checks passed"
