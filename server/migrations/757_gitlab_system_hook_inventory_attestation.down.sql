DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM vcs_gitlab_migration_system_hook_attestation) THEN
        RAISE EXCEPTION 'refusing to drop GitLab System Hook attestation audit records';
    END IF;
END
$$;

DROP TABLE vcs_gitlab_migration_system_hook_attestation;
