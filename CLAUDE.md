@AGENTS.md

## Claude Code

- The hooks in `.claude/settings.json` enforce the generated-file rule and the lint check. If a hook blocks you, fix the finding. Do not look for a way around the hook.
- Read a file from `docs/` only when you change that topic. `README.md` is for users, not for you.
- Use the `Explore` agent for searches over many files. It runs on a small model.

# Compact instructions

Keep the names of failing tests, the list of changed files and the current task. Drop passing test output.
