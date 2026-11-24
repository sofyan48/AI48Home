# Documentation

AI48Home documentation is maintained separately from the upstream CLIProxyAPIHome documentation to reduce conflicts when merging upstream changes.

## AI48Home documentation

Use these documents for this repository's API behavior, including AI48Home additions and overrides:

- Management API: [English](ai48home/management/api.md) | [中文](ai48home/management/api_CN.md)
- User API: [English](ai48home/user/api.md) | [中文](ai48home/user/api_CN.md)
- [Alerting](ai48home/management/alerting.md)
- [User analytics](ai48home/management/user-analytics.md)

The API documents under `ai48home/` are complete AI48Home references, not just lists of differences from upstream. Their migration preserves the existing fork documentation, including local edits.

## Upstream documentation

These files remain at their original upstream paths:

- Management API: [English](management/api.md) | [中文](management/api_CN.md)
- User API: [English](user/api.md) | [中文](user/api_CN.md)

They describe [CLIProxyAPIHome](https://github.com/router-for-me/CLIProxyAPIHome), not necessarily AI48Home's current API. At the initial separation, they were restored from upstream commit `9208558`; future upstream merges can update them normally.

## Maintenance rules

1. Put AI48Home-specific documentation changes and new guides under `docs/ai48home/`. Keep matching language-specific API documents in sync when applicable.
2. Do not add fork-specific content to `docs/management/` or `docs/user/`, and do not move the upstream files. Keeping their paths and content upstream-owned avoids recurring content or rename/modify conflicts.
3. When merging upstream, let upstream documentation changes land at their original paths. Review API changes against AI48Home's current handlers and manually incorporate applicable changes into `docs/ai48home/`.
4. Link to `docs/ai48home/` from AI48Home documentation and contributor instructions. Relative links within the AI48Home documentation tree retain the same management/user directory layout.

This separation reduces documentation conflicts; it does not automatically synchronize the two documentation sets or prevent conflicts in other shared files.
