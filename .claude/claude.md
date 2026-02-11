# Project-Specific Guidelines

## File Modification Priority

IMPORTANT: When making code changes, prioritize files that are already modified (dirty) in the git history before changing any clean files.

- First check `git status` to identify modified files
- When implementing changes, prefer modifying already-dirty files over clean ones
- Only modify clean files when absolutely necessary for the task
- This helps keep changesets focused and easier to review
