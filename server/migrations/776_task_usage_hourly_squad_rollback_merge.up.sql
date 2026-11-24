-- Forward no-op: pairs with 776 down, which merges split squad_id buckets back
-- into squad_id = NULL before 775/774 down rebuild the legacy 7-column unique
-- indexes CONCURRENTLY.
SELECT 1;
