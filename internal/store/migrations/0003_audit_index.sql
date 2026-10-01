-- The audit log is read newest first by id, a page at a time. An index on
-- the time could not serve that, so each page sorted all of an
-- organisation's rows.
DROP INDEX audit_org_idx;
CREATE INDEX audit_org_id_idx ON audit (org_id, id DESC);
