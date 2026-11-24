-- Its primary key is deferred to migration 768, after concurrent index build 767.
CREATE TABLE vcs_gitlab_migration_system_hook_attestation (
    generation BIGINT NOT NULL CHECK (generation > 0),
    instance_url TEXT NOT NULL CHECK (
        left(instance_url, 8) = 'https://'
        AND length(instance_url) > 8
        AND strpos(substr(instance_url, 9), '/') = 0
        AND strpos(substr(instance_url, 9), '?') = 0
        AND strpos(substr(instance_url, 9), '#') = 0
        AND strpos(substr(instance_url, 9), '@') = 0
        AND instance_url !~ '[[:space:]]'
    ),
    operator_identity TEXT NOT NULL CHECK (btrim(operator_identity) <> ''),
    attestation TEXT NOT NULL CHECK (attestation = 'no_legacy_multica_system_hook'),
    attested_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
