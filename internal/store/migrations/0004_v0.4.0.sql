-- What v0.4.0 changes in the schema.

-- What someone attached to a chat message in Keera Chat: images and files,
-- as protocol.Attachments. NULL is none. Only the runtime reads it, so lists
-- of tasks never load it. See docs/tasks.md.
ALTER TABLE tasks ADD COLUMN attachments jsonb;
