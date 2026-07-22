-- 041_create_oracle.down.sql

-- oracle_fires first (FK -> decrees), then decrees and vigils.
DROP TABLE IF EXISTS oracle_fires;
DROP TABLE IF EXISTS decrees;
DROP TABLE IF EXISTS vigils;
