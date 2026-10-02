-- API connectors, such as Jira or AWS, bind as read or write.
ALTER TABLE bindings DROP CONSTRAINT bindings_mode_check;
ALTER TABLE bindings ADD CONSTRAINT bindings_mode_check
    CHECK (mode IN ('chat', 'watch', 'files', 'repo', 'read', 'write'));
