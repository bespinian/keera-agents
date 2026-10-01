-- A card or schedule starts work for whoever last gave it, not whoever
-- created it: otherwise anyone could edit another person's card and have it
-- run with that person's rights.
ALTER TABLE cards
    ADD COLUMN requested_by    text NOT NULL DEFAULT '',
    ADD COLUMN requested_by_id text NOT NULL DEFAULT '';
UPDATE cards SET requested_by = created_by, requested_by_id = created_by_id;

ALTER TABLE schedules
    ADD COLUMN requested_by    text NOT NULL DEFAULT '',
    ADD COLUMN requested_by_id text NOT NULL DEFAULT '';
UPDATE schedules SET requested_by = created_by, requested_by_id = created_by_id;
