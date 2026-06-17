-- 041_create_oracle.down.sql

-- oracle_fires первым (FK → decrees), затем decrees и vigils.
DROP TABLE IF EXISTS oracle_fires;
DROP TABLE IF EXISTS decrees;
DROP TABLE IF EXISTS vigils;
