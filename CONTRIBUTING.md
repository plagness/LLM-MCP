# Contributing

## Branch and commits
- Create feature branches from `main`.
- Keep commits focused and descriptive.
- Use version format `YYYY.MM.x` for release updates.

## Before opening PR
1. Run local checks:
   - `docker compose -f compose.yml config`
   - Python syntax check for touched Python code
   - Any module-specific smoke checks from `README.md`
2. Update docs when behavior changes.
3. Update `CHANGELOG.md` and `VERSION` for release-facing changes.

## Pull request checklist
- [ ] Behavior change is documented
- [ ] `CHANGELOG.md` updated
- [ ] `VERSION` updated when needed
- [ ] No secrets in tracked files
