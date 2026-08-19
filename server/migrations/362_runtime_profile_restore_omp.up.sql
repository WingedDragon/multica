-- Re-add OMP (`omp`) to the runtime_profile protocol_family CHECK.
-- Migration 342_runtime_profile_add_mcode rebuilt the constraint from
-- upstream's whitelist, which registers omp as a pi-family runtime identity
-- rather than a protocol family, so this fork's 314 addition was silently
-- dropped from the live constraint. Here omp is a first-class protocol
-- family (agent.SupportedTypes keeps it, and the handler validates against
-- that list), so restore it alongside mcode. Kept in lockstep with
-- agent.SupportedTypes. NOT VALID preserves the historical-row tolerance
-- used by every prior rebuild of this constraint.
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
        'mcode'
    )) NOT VALID;
