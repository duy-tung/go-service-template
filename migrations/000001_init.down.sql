-- No BEGIN/COMMIT here: apply with psql -1 (see Makefile migrate-down).

DROP TABLE orders;
DROP TABLE accounts;
