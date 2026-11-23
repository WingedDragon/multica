-- A prior down migration may have restored host uniqueness as a standalone concurrent index.
-- Remove it after 743 so re-up permits multiple base URLs on one host again.
DROP INDEX CONCURRENTLY IF EXISTS gitlab_connection_workspace_host_uidx;
