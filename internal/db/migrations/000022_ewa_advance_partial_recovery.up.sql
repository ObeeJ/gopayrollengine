ALTER TABLE ewa_advances ADD COLUMN recovered_kobo BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ewa_advances ADD CONSTRAINT ewa_advances_recovered_kobo_bounds
    CHECK (recovered_kobo >= 0 AND recovered_kobo <= amount_kobo);
