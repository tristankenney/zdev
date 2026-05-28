#!/usr/bin/env bash
# drift-check.sh — Phase 4 D4-17: byte-equality drift harness for 14 VIS scenarios.
# CONTEXT D4-17: bash deterministic-mode drift detection vs Go-self goldens.
# CONTEXT D4-18: outcome populates DRIFT-FINDINGS.md; Plan 04-09 reads it to gate cutover.
# Pitfall 4 (04-RESEARCH.md): the deterministic-mode patch must normalize width,
# seed time, bypass cksum palette, fix sessions. This harness sets all four
# via env vars per scenario.
#
# Usage:
#   cd zdevd && make drift-check
#   # Or directly:
#   bash scripts/drift-check.sh
#
# Exit code:
#   0  — all 14 scenarios byte-equal (CLEAN)
#   1  — at least one scenario differs (DRIFT DETECTED; see DRIFT-FINDINGS.md)
#   2  — setup error (missing bash binary, missing patch, missing jq, etc.)
#
# Requirements:
#   - ~/.local/bin/zdev-sidebar-render-bash (or $ZDEVD_BASH_BIN) with the
#     deterministic-mode patch from Plan 04-08 Task 1 applied.
#   - jq must be available on PATH.
#   - 14 VIS golden + snapshot fixture pairs in internal/render/testdata/golden/.
set -euo pipefail

# Resolve root to zdevd/ directory regardless of working directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ZDEVD_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${ZDEVD_DIR}/.." && pwd)"

cd "${ZDEVD_DIR}"

GOLDEN_DIR="internal/render/testdata/golden"
BASH_BIN="${ZDEVD_BASH_BIN:-${HOME}/.local/bin/zdev-sidebar-render-bash}"
FINDINGS_REL=".planning/phases/04-hardening-cutover/DRIFT-FINDINGS.md"
FINDINGS="${REPO_ROOT}/${FINDINGS_REL}"

# Verify prerequisites.
if [ ! -x "${BASH_BIN}" ]; then
    echo "drift-check: ${BASH_BIN} missing or not executable" >&2
    echo "  Apply the deterministic-mode patch (Plan 04-08 Task 1) and try again." >&2
    exit 2
fi
if ! grep -q 'ZDEV_SIDEBAR_DETERMINISTIC' "${BASH_BIN}" 2>/dev/null; then
    echo "drift-check: ${BASH_BIN} does not have the deterministic-mode patch applied" >&2
    echo "  Run: cd \$HOME/.local/bin && patch -p0 zdev-sidebar-render-bash < zdev-sidebar-render-bash.deterministic-mode.patch" >&2
    exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "drift-check: jq is required but not found on PATH" >&2
    exit 2
fi

RUN_TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Write FINDINGS header (overwrite each run; this file is a verdict artifact).
cat > "${FINDINGS}" <<EOF
# Drift Findings — Phase 4 D4-17

**Run:** ${RUN_TS}
**Bash baseline:** ${BASH_BIN}
**Goldens:** ${GOLDEN_DIR}/vis-{01..14}.golden

**Purpose:** Compares deterministic-mode bash renderer output against
Go-self-generated goldens for each of the 14 VIS scenarios. Each DRIFT
entry requires operator classification (intentional vs unintentional) before
cutover (Plan 04-09) can proceed.

**Note on expected drift:** The bash renderer and Go renderer have known
architectural differences (bash uses POSIX cksum for palette; Go uses a
custom POSIX cksum reimplementation; animation frame, header format, metadata
line format, and chip ordering may differ). First-run DRIFT is expected.
Operator classifies each divergence.

## Scenarios

EOF

overall=0  # 0 = all pass; 1 = at least one drift

for n in 01 02 03 04 05 06 07 08 09 10 11 12 13 14; do
    snap="${GOLDEN_DIR}/vis-${n}.snapshot.json"
    gold="${GOLDEN_DIR}/vis-${n}.golden"

    if [ ! -f "${snap}" ] || [ ! -f "${gold}" ]; then
        echo "  - VIS-${n}: SKIP — fixture or golden missing (${snap} or ${gold})" >> "${FINDINGS}"
        continue
    fi

    # WIDTH: read from snapshot .width if present, otherwise 50 (matches Go test default).
    WIDTH=$(jq -r 'if .width then .width else 50 end' "${snap}" 2>/dev/null || echo "50")

    # NOW: use a fixed deterministic epoch so age calculations are stable.
    # The golden was rendered at a fixed point in time; use the same epoch.
    # sent_at is "2026-05-04T12:00:00Z" = 1746360000 epoch.
    # Use a value that makes all age-related scenarios produce the same output
    # as the Go renderer which uses the sent_at time as "now" for age calculations.
    NOW=$(jq -r 'if .now then .now else 1746360000 end' "${snap}" 2>/dev/null || echo "1746360000")

    # SESSIONS + PROJECTS: build from snapshot .projects[].status.
    # Each project becomes a session with the same status.
    SESSIONS=""
    PROJECTS=""
    CURRENT_SESSION=""

    while IFS=$'\t' read -r pname pstatus; do
        [ -z "${pname}" ] && continue
        # Map Go status names to bash harness status tokens.
        case "${pstatus}" in
            waiting)       bstatus="waiting" ;;
            shell-running) bstatus="shell-running" ;;
            finished)      bstatus="finished" ;;
            alive)         bstatus="alive" ;;
            absent)        bstatus="absent" ;;
            *)             bstatus="alive" ;;
        esac
        [ -n "${SESSIONS}" ] && SESSIONS+=","
        SESSIONS+="${pname}:${bstatus}"
        [ -n "${PROJECTS}" ] && PROJECTS+=$'\n'
        PROJECTS+="${pname}"
    done < <(jq -r '.projects[]? | [.name, .status] | @tsv' "${snap}" 2>/dev/null)

    CURRENT_SESSION=$(jq -r '.current_session // ""' "${snap}" 2>/dev/null || echo "")

    # PALETTE: compute cksum-of-name for each project to reproduce bash's
    # palette assignment. This bypasses any POSIX vs BSD cksum divergence by
    # using the host's cksum (same as what the bash script uses in production).
    PALETTE_EXPORTS=""
    while IFS=$'\t' read -r pname; do
        [ -z "${pname}" ] && continue
        h=$(printf '%s' "${pname}" | cksum | awk '{print $1}')
        PALETTE=(39 45 51 75 81 87 105 111 141 147 177 183 207 213 219)
        idx=$(( h % ${#PALETTE[@]} ))
        sanitized="${pname//[^A-Za-z0-9_]/_}"
        PALETTE_EXPORTS+=" ZDEV_DETERMINISTIC_PALETTE_${sanitized}=${idx}"
    done < <(jq -r '.projects[]?.name' "${snap}" 2>/dev/null)

    # NOTIF_DIR: create a temp dir for any wait-age notif files this scenario needs.
    notif_dir=$(mktemp -d)

    # WAIT_AGES: if snapshot has notif timestamps (sessions with waiting status),
    # create notif files to simulate wait-state ages. For VIS scenarios without
    # explicit notif data, we don't create notif files (age = 0 / undefined).
    if jq -e '.notifs' "${snap}" >/dev/null 2>&1; then
        jq -r '.notifs[]? | [.session, .ts] | @tsv' "${snap}" 2>/dev/null \
            | while IFS=$'\t' read -r sess ts; do
                printf '%s\n' "${ts}" > "${notif_dir}/zdev-notif-${sess}.ts" || true
            done
    fi

    # Run the deterministic bash renderer (one-shot via _DET_ONE_SHOT=1).
    captured=$(mktemp)
    # shellcheck disable=SC2086
    if env -i \
        HOME="${HOME}" \
        PATH="${PATH}" \
        ZDEV_SIDEBAR_DETERMINISTIC=1 \
        ZDEV_SIDEBAR_WIDTH="${WIDTH}" \
        ZDEV_DETERMINISTIC_NOW="${NOW}" \
        ZDEV_DETERMINISTIC_SESSIONS="${SESSIONS}" \
        ZDEV_DETERMINISTIC_PROJECTS="${PROJECTS}" \
        ZDEV_DETERMINISTIC_CURRENT_SESSION="${CURRENT_SESSION}" \
        ZDEV_NOTIF_DIR="${notif_dir}" \
        ${PALETTE_EXPORTS} \
        "${BASH_BIN}" 2>/dev/null > "${captured}"; then
        : # bash exited cleanly (exit 0)
    else
        bash_exit="$?"
        {
            printf '  - VIS-%s: BASH_FAILED — exit %s\n' "${n}" "${bash_exit}"
            printf '    (possibly missing env var or unsupported scenario)\n'
        } >> "${FINDINGS}"
        overall=1
        rm -rf "${notif_dir}" "${captured}"
        continue
    fi

    # Byte-equality comparison.
    if cmp -s "${captured}" "${gold}"; then
        printf '  - VIS-%s: PASS — byte-equal\n' "${n}" >> "${FINDINGS}"
    else
        overall=1
        diff_out=$(diff <(xxd "${gold}") <(xxd "${captured}") 2>/dev/null | head -30 || true)
        {
            printf '  - VIS-%s: DRIFT\n' "${n}"
            printf '    Captured: %s (preserved for manual inspection)\n' "${captured}"
            printf '    Golden:   %s\n' "${gold}"
            printf '    Captured size: %s bytes; Golden size: %s bytes\n' \
                "$(wc -c < "${captured}" | tr -d ' ')" \
                "$(wc -c < "${gold}" | tr -d ' ')"
            printf '    First differing bytes (xxd diff):\n'
            printf '    ```\n'
            printf '%s\n' "${diff_out}"
            printf '    ```\n'
            printf '    Classification: TODO\n'
            printf '      Intentional drift: update golden via `cd zdevd && go test -run TestVisualParity ./internal/render/... -update`, document rationale here.\n'
            printf '      Unintentional drift: fix the Go renderer (or the bash patch), re-run drift-check.\n'
        } >> "${FINDINGS}"
        # Keep captured file if drift (tmpfs cleans on reboot); don't rm.
    fi

    if cmp -s "${captured}" "${gold}"; then
        rm -f "${captured}"
    fi
    rm -rf "${notif_dir}"
done

# Write FINDINGS trailer.
{
    printf '\n'
    if [ "${overall}" = "0" ]; then
        printf '## Overall: CLEAN\n\n'
        printf 'All 14 VIS scenarios byte-equal between deterministic-mode bash and Go-self goldens.\n'
        printf 'SC1-OVERRIDE.md may flip status: active -> superseded (Plan 04-09 D4-18).\n'
    else
        printf '## Overall: DRIFT DETECTED\n\n'
        printf 'At least one scenario differs. Operator must classify each DRIFT entry above:\n\n'
        printf '  - **Intentional drift:** update Go golden via:\n'
        printf '    ```\n'
        printf '    cd zdevd && go test -run TestVisualParity ./internal/render/... -update\n'
        printf '    ```\n'
        printf '    Document the rationale in this file alongside the scenario entry.\n\n'
        printf '  - **Unintentional drift:** fix the Go renderer (or the bash deterministic patch\n'
        printf '    if the bash output is wrong), re-run `make drift-check`.\n\n'
        printf 'Cutover (Plan 04-09) BLOCKED until all drift entries are classified and either:\n'
        printf '  (a) goldens updated + rationale documented, or\n'
        printf '  (b) zero-drift achieved.\n'
    fi
} >> "${FINDINGS}"

echo "drift-check: findings written to ${FINDINGS_REL}"
exit "${overall}"
