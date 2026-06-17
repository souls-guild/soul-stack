-- 071_create_heralds_tidings.down.sql

-- tidings раньше heralds — FK tidings.herald → heralds(name).
DROP TABLE IF EXISTS tidings;
DROP TABLE IF EXISTS heralds;
