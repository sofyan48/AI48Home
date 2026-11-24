# Organization storage migration (schema v28)

Organizations now have a dedicated `organization` table. They are no longer stored as login users. Organization rows contain organization settings and billing policy/state, not passwords, email credentials, MFA, or passkeys.

## Billing and usage compatibility

- `billing_identity` allocates a shared, permanent payer ID namespace for personal users and organizations. This prevents collisions between independently stored owners.
- Existing organization IDs are preserved, including soft-deleted organizations. Existing API-key ownership, charge ownership, member attribution, and usage records are not renumbered or recalculated.
- For API compatibility, existing payer fields named `user_id` retain their historical meaning: a personal or organization payer ID. `created_by_user_id` and `actor_user_id` continue to identify personal users.
- Balances, unlimited-credit settings, billing multipliers, period limits, window state, usage epochs, reset allowances, timestamps, and organization/member policies are preserved.
- Charging and top-up approval resolve the payer's actual table. Billing and usage reports include both personal and organization payers; member reports still resolve personal users only.
- Repository user operations target personal users. Organization management uses dedicated organization operations. Legacy `UserRecord` values returned by organization APIs are compatibility DTOs and must never be persisted to the `user` table.

## Upgrade procedure

1. Back up the database before upgrading.
2. Stop **all** Home processes writing to the database. Do not run old and new Home binaries together during or after this migration: old binaries expect organizations in `user`.
3. Start the updated Home binary. The coordinated schema migration creates the new tables, copies organization state, reserves existing payer IDs, removes migrated organization rows from `user`, and repairs the identity sequence transactionally.
4. Verify organization balances and billing/usage reports before resuming traffic and starting other updated Home instances.

Portable snapshot format v28 includes the new tables. Older supported snapshots are converted on import after validating source record counts and before validating the new relationships. Do not restore a v28 database into an older Home binary. Rollback requires restoring the pre-upgrade backup while all writers are stopped.

## Organization credential scope (schema/snapshot v29)

`organization.organization_channel_groups` persists the organization's credential scope; the compatibility `UserRecord` DTO carries it without adding a column to `user`. The coordinated schema gate advances from v28 to v29. Existing rows have NULL scope, equivalent to `[]` (unrestricted); no historical key bindings are rewritten merely by upgrading. Legacy organization conversion omits this organization-only column.

Management create/update accepts `channel_groups` and create/update/list/detail returns an array. Setting the scope rewrites all organization-owned key `channels`, including soft-deleted and administrator-created keys, atomically under the existing key mutation lock and publishes a configuration event. Management updates refresh runtime config. Key creation, restore, reassignment, binding updates and full-list replacement enforce inheritance; runtime dispatch and model catalogs also resolve the organization policy rather than trusting stale key bindings. Member model-scope rewrites preserve credential scopes. Personal keys are unchanged.

Omitted/null create scope is unrestricted; omitted/null PATCH scope is unchanged; `[]` explicitly clears restrictions. Deleted scopes cannot be rebound, and deletion cleans organization scope alongside existing key bindings. Disabled/unknown scopes retain the existing restrictive behavior. Model-detail channel scopes continue to intersect the organization credential scope.

Snapshot v29 preserves the new column. Supported older snapshots, including v28, remain importable and missing scope decodes as unrestricted. Stop old writers before upgrading; old binaries do not enforce organization credential policies. Rollback requires the pre-upgrade backup.
