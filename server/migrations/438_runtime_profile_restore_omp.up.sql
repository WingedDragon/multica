-- Re-add OMP (`omp`) to the runtime_profile protocol_family CHECK.
-- Same regression as migration 362: upstream rebuilds this constraint from its
-- own whitelist, where omp is not a protocol family, so this fork's entry is
-- silently dropped. Migration 370 (dim) and migration 403 (zeroclaw) each
-- rebuilt it again after 362, so the live constraint lost omp a second time and
-- rejected every new OMP runtime_profile row.
--
-- Here omp is a first-class protocol family (agent.SupportedTypes keeps it and
-- the handler validates against that list), so restore it alongside dim and
-- zeroclaw. Kept in lockstep with agent.SupportedTypes, which
-- TestSupportedTypesMatchesMigrationWhitelist now verifies by parsing the
-- newest constraint migration instead of a hand-copied list. NOT VALID
-- preserves the historical-row tolerance used by every prior rebuild.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'openclaw',
        'hermes',
        'pi',
        'omp',
        'cursor',
        'kimi',
        'reasonix',
        'dsh',
        'kiro',
        'antigravity',
        'qoder',
        'qoderclicn',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'qwenpaw',
        'mcode',
        'dim',
        'zeroclaw'
    )) NOT VALID;
