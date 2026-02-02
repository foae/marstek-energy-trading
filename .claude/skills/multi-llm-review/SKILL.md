---
name: multi-llm-review
description: Review completed implementation against PRD using multiple external LLMs (Gemini, Codex). Use after finishing implementation tasks, when all code changes are complete, when preparing for code review, or when user asks to "review implementation", "get external review", "check my work", or "multi-llm review".
allowed-tools: Bash, Read, Glob, Grep, Task, AskUserQuestion
---

# Multi-LLM Implementation Review

## Goal

Obtain external, objective reviews of an implementation from multiple LLMs (Gemini and Codex), then rigorously validate each finding against the actual codebase before presenting actionable items to the user.

## Workflow

### Step 1: Detect the PRD file

Find the most recently modified PRD file:

```bash
ls -t docs/*-prd.md 2>/dev/null | head -1
```

If no PRD file found, use `AskUserQuestion` to ask the user:
- "No PRD file found in docs/. Please provide the path to the PRD file or describe what should be reviewed."

### Step 2: Run multi-LLM reviews

Execute the review script with the PRD file path:

```bash
bash .claude/skills/multi-llm-review/scripts/run-reviews.sh <prd-file-path>
```

This runs Gemini and Codex reviews in parallel. Both must succeed for the review to complete.

If the script fails, inform the user of the error (e.g., missing CLI tool).

### Step 3: Process the reviews

For each finding from both Gemini and Codex:

1. **Research**: Use Glob, Grep, and Read to investigate the concern in the actual codebase
2. **Validate**: Determine if the concern is legitimate based on the implemented code
3. **Cross-reference**: Check if findings from different LLMs overlap (higher confidence if both found it)
4. **Categorize**:
   - **VALID** - The concern is legitimate and needs addressing
   - **INVALID** - The concern is based on incorrect assumptions or already handled in code
   - **NEEDS_CLARIFICATION** - Cannot determine validity without user input

### Step 4: Present actionable items

Only present VALID findings to the user. Format as:

```
## Multi-LLM Review Summary

PRD reviewed: `docs/xxx-prd.md`
Reviewers: Gemini, Codex

### Actionable Items

#### 1. [HIGH] Issue title
**Sources**: Gemini, Codex (or single source)
**Finding**: <consolidated description>
**Location**: <file:line or component>
**Your validation**: <what you found in the codebase that confirms this>
**Suggested fix**: <brief recommendation>

#### 2. [MEDIUM] Issue title
...

### Dismissed Items (for transparency)

| # | Issue | Source | Why Dismissed |
|---|-------|--------|---------------|
| 1 | ... | Gemini | Already handled in xyz.go:123 |
| 2 | ... | Codex | Incorrect assumption about ... |
```

### Step 5: Get user decision

Use `AskUserQuestion` to let the user choose which items to address:

For each VALID item, offer options:
- **Fix now** - Implement the fix
- **Skip** - Acknowledge but don't fix
- **Discuss** - Need more context before deciding

### Step 6: Implement selected fixes

For items the user chose to fix:
1. Implement the fix following existing code patterns
2. Ensure tests pass
3. Summarize what was changed

## Rules

- Do NOT blindly accept findings - verify each one against the actual code
- Do NOT modify any files during the review phase (Steps 1-5)
- Only implement fixes the user explicitly approved
- If reviewers disagree, present both perspectives and let user decide
- Cross-referenced findings (both LLMs agree) should be flagged as higher confidence
