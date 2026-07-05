-- Seeded before the first backup (runs against MARIADB_DATABASE=e2edb);
-- the restore roundtrip drops this table and asserts the restore brings
-- the row back.
CREATE TABLE e2e_marker (v VARCHAR(64));
INSERT INTO e2e_marker VALUES ('roundtrip-ok');
