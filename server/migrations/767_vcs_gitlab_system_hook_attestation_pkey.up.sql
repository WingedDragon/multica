CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS vcs_gitlab_migration_system_hook_attestation_pkey
    ON vcs_gitlab_migration_system_hook_attestation (generation, instance_url);
