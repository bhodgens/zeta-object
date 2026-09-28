#!/bin/bash
#
# show-bucket-actions.sh - Display all .bucket-actions configurations in a directory tree
#
# Usage: ./show-bucket-actions.sh [path]
#        path - Directory to search (default: current directory)
#
# This script finds all .bucket-actions files and displays a summary of their
# configured actions, including action names, patterns, and inactivity settings.

set -e

ROOT="${1:-.}"

# Colors for output (if terminal supports them)
if [[ -t 1 ]]; then
    BOLD='\033[1m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    CYAN='\033[0;36m'
    NC='\033[0m' # No Color
else
    BOLD=''
    GREEN=''
    YELLOW=''
    CYAN=''
    NC=''
fi

# Strip JSON5 comments for jq processing — string-aware: // and /* */ inside
# quoted strings are preserved. Uses a small python3 state machine mirroring
# the server's stripJSON5Comments; falls back to the old (naive) sed with a
# warning if python3 is unavailable.
strip_comments() {
    if command -v python3 &>/dev/null; then
        python3 -c '
import sys
out = []
data = sys.stdin.buffer.read().decode("utf-8", errors="replace")
i, n = 0, len(data)
in_str = in_line = in_block = False
while i < n:
    c = data[i]
    if in_line:
        if c == "\n":
            in_line = False
            out.append(c)
        i += 1
        continue
    if in_block:
        if c == "*" and i + 1 < n and data[i + 1] == "/":
            in_block = False
            i += 2
            continue
        if c == "\n":
            out.append(c)
        i += 1
        continue
    if in_str:
        if c == "\\" and i + 1 < n:
            out.append(c)
            out.append(data[i + 1])
            i += 2
            continue
        if c == "\"":
            in_str = False
        out.append(c)
        i += 1
        continue
    if c == "/" and i + 1 < n:
        nxt = data[i + 1]
        if nxt == "/":
            in_line = True
            i += 2
            continue
        if nxt == "*":
            in_block = True
            i += 2
            continue
    if c == "\"":
        in_str = True
    out.append(c)
    i += 1
sys.stdout.write("".join(out))
'
    else
        echo "WARNING: python3 not found; using naive sed comment stripper (comments inside strings may corrupt output)" >&2
        sed -e 's|//.*$||g' -e ':a;N;$!ba;s|/\*[^*]*\*\+\([^/*][^*]*\*\+\)*/||g'
    fi
}

# Check if jq is available
HAS_JQ=false
if command -v jq &>/dev/null; then
    HAS_JQ=true
fi

# Find all .bucket-actions files
echo -e "${BOLD}Scanning for .bucket-actions files in: $ROOT${NC}"
echo ""

found=0
while IFS= read -r -d '' file; do
    found=$((found + 1))
    echo -e "${BOLD}${CYAN}=== $file ===${NC}"

    if $HAS_JQ; then
        # Use jq for pretty parsing
        content=$(strip_comments < "$file")

        # Extract and display action summaries
        echo -e "${GREEN}after_upload:${NC}"
        echo "$content" | jq -r '.after_upload // [] | .[] | "  - \(.name)\(.enabled == false | if . then " (disabled)" else "" end)\(if .patterns then " [" + (.patterns | join(", ")) + "]" else "" end)"' 2>/dev/null || echo "  (none or parse error)"

        echo -e "${GREEN}after_download:${NC}"
        echo "$content" | jq -r '.after_download // [] | .[] | "  - \(.name)\(.enabled == false | if . then " (disabled)" else "" end)\(if .patterns then " [" + (.patterns | join(", ")) + "]" else "" end)"' 2>/dev/null || echo "  (none or parse error)"

        echo -e "${GREEN}after_delete:${NC}"
        echo "$content" | jq -r '.after_delete // [] | .[] | "  - \(.name)\(.enabled == false | if . then " (disabled)" else "" end)\(if .patterns then " [" + (.patterns | join(", ")) + "]" else "" end)"' 2>/dev/null || echo "  (none or parse error)"

        echo -e "${GREEN}inactivity_timeout:${NC}"
        inactivity=$(echo "$content" | jq -r '.inactivity_timeout | if . then "\(.duration)\(if .enabled == false then " (disabled)" else "" end) - \(.description // "no description")" else "none" end' 2>/dev/null)
        echo "  $inactivity"

        echo -e "${GREEN}inheritance:${NC}"
        inheritance=$(echo "$content" | jq -r '.inheritance.mode // "merge (default)"' 2>/dev/null)
        echo "  $inheritance"
    else
        # Fallback: just show the file contents
        echo -e "${YELLOW}(jq not available - showing raw content)${NC}"
        cat "$file"
    fi

    echo ""
done < <(find "$ROOT" -name ".bucket-actions" -print0 2>/dev/null)

if [[ $found -eq 0 ]]; then
    echo "No .bucket-actions files found in $ROOT"
    exit 0
fi

echo -e "${BOLD}Found $found .bucket-actions file(s)${NC}"
