-- Restore the runtime_profile whitelist to the daemon's complete SupportedTypes set.
-- Migration 656's replacement constraint omitted OMP and Museapp; keep this
-- constraint aligned without scanning historical rows.
ALTER TABLE runtime_profile
    DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check,
    ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'codearts',
        'deveco',
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
        'grok',
        'qwen',
        'qwenpaw',
        'droid',
        'amp',
        'mcode',
        'muse',
        'dim',
        'devin',
        'zeroclaw',
        'museapp',
        'zcode'
    )) NOT VALID;
