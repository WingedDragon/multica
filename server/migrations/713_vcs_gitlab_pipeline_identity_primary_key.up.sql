ALTER TABLE vcs_gitlab_pipeline
    ADD CONSTRAINT vcs_gitlab_pipeline_pkey PRIMARY KEY USING INDEX vcs_gitlab_pipeline_identity_uidx;
