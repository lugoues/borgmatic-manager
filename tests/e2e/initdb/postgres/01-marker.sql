-- Seeded before the first backup; the restore roundtrip drops this table
-- and asserts the restore brings the row back.
CREATE TABLE e2e_marker (v text);
INSERT INTO e2e_marker VALUES ('roundtrip-ok');
