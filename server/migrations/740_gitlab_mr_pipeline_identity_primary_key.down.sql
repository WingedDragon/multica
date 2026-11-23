ALTER TABLE gitlab_mr_pipeline
    DROP CONSTRAINT gitlab_mr_pipeline_pkey;

ALTER TABLE gitlab_mr_pipeline
    ADD CONSTRAINT gitlab_mr_pipeline_pkey PRIMARY KEY USING INDEX gitlab_mr_pipeline_legacy_identity_uidx;
