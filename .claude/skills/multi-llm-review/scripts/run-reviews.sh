#!/usr/bin/env bash
set -euo pipefail

PRD_FILE="${1:-}"

# Validate PRD file
if [[ -z "$PRD_FILE" ]]; then
    echo "Error: PRD file path required as argument" >&2
    exit 1
fi

if [[ ! -f "$PRD_FILE" ]]; then
    echo "Error: PRD file not found: $PRD_FILE" >&2
    exit 1
fi

# Check if required CLI tools are installed
MISSING_TOOLS=()

if ! command -v gemini &> /dev/null; then
    MISSING_TOOLS+=("gemini")
fi

if ! command -v codex &> /dev/null; then
    MISSING_TOOLS+=("codex")
fi

if [[ ${#MISSING_TOOLS[@]} -gt 0 ]]; then
    echo "Error: Required CLI tools not found: ${MISSING_TOOLS[*]}" >&2
    echo "Please install the missing tools and ensure they are in your PATH." >&2
    exit 1
fi

# Read PRD content
PRD_CONTENT=$(cat "$PRD_FILE")

# Build review prompt
REVIEW_PROMPT="You are a staff software engineer performing a code review.

CRITICAL: You are in READ-ONLY mode. Do NOT write, create, or modify any files.
Your ONLY task is to review and output findings as text in MARKDOWN format.

## Context

The implementation you are reviewing is in the CURRENT BRANCH of this repository.
The Product Requirements Document (PRD) describing what should be implemented is provided below.

## Your Task

1. Explore the codebase to understand what has been implemented
2. Compare the implementation against the PRD requirements
3. Identify any issues, bugs, missing features, or concerns

## Review Categories

Look for issues in these categories:
1. **Correctness**: Does the implementation match PRD requirements?
2. **Bugs**: Logic errors, edge cases, potential runtime errors
3. **Security**: Vulnerabilities, unsafe practices
4. **Performance**: Inefficiencies, potential bottlenecks
5. **Code Quality**: Maintainability, patterns, clarity

## Output Format

For each issue found, output in this MARKDOWN format:

## [NUMBER]. [SEVERITY: HIGH/MEDIUM/LOW] Short title

**Category**: (one of the 5 above)

**Description**: Detailed explanation of the concern

**Location**: File path and line number or function name

**Recommendation**: Suggested fix or approach

---

If the implementation is solid and matches the PRD with no issues, state that clearly.

## Important

- Your review will be assessed by another LLM, so write findings concisely and clearly
- Rate each finding for criticality (HIGH/MEDIUM/LOW)
- Be specific about locations - include file paths and line numbers when possible
- Focus on actionable items, not style preferences

---

## PRD (Product Requirements Document)

$PRD_CONTENT"

# Create temp files for outputs
GEMINI_OUTPUT=$(mktemp)
CODEX_OUTPUT=$(mktemp)
GEMINI_EXIT=$(mktemp)
CODEX_EXIT=$(mktemp)

# Cleanup on exit
cleanup() {
    rm -f "$GEMINI_OUTPUT" "$CODEX_OUTPUT" "$GEMINI_EXIT" "$CODEX_EXIT"
}
trap cleanup EXIT

# Run Gemini review in background
(
    gemini --sandbox --prompt "$REVIEW_PROMPT" < "$PRD_FILE" > "$GEMINI_OUTPUT" 2>&1
    echo $? > "$GEMINI_EXIT"
) &
GEMINI_PID=$!

# Run Codex review in background
(
    echo "$REVIEW_PROMPT" | codex exec --sandbox read-only - > "$CODEX_OUTPUT" 2>&1
    echo $? > "$CODEX_EXIT"
) &
CODEX_PID=$!

# Wait for both to complete
wait $GEMINI_PID 2>/dev/null || true
wait $CODEX_PID 2>/dev/null || true

# Check exit codes
GEMINI_RESULT=$(cat "$GEMINI_EXIT")
CODEX_RESULT=$(cat "$CODEX_EXIT")

# Report failures
FAILED=()
if [[ "$GEMINI_RESULT" != "0" ]]; then
    FAILED+=("Gemini (exit code: $GEMINI_RESULT)")
fi
if [[ "$CODEX_RESULT" != "0" ]]; then
    FAILED+=("Codex (exit code: $CODEX_RESULT)")
fi

if [[ ${#FAILED[@]} -gt 0 ]]; then
    echo "Error: Review failed for: ${FAILED[*]}" >&2
    echo "" >&2
    if [[ "$GEMINI_RESULT" != "0" ]]; then
        echo "=== Gemini Error Output ===" >&2
        cat "$GEMINI_OUTPUT" >&2
    fi
    if [[ "$CODEX_RESULT" != "0" ]]; then
        echo "=== Codex Error Output ===" >&2
        cat "$CODEX_OUTPUT" >&2
    fi
    exit 1
fi

# Output combined results
echo "# Multi-LLM Review Results"
echo ""
echo "PRD File: \`$PRD_FILE\`"
echo ""
echo "---"
echo ""
echo "## Gemini Review"
echo ""
cat "$GEMINI_OUTPUT"
echo ""
echo "---"
echo ""
echo "## Codex Review"
echo ""
cat "$CODEX_OUTPUT"
