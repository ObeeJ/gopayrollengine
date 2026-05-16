-- 000004_users_and_advances.down.sql
DROP INDEX IF EXISTS idx_advances_status;
DROP INDEX IF EXISTS idx_advances_org;
DROP INDEX IF EXISTS idx_advances_employee;
DROP TABLE IF EXISTS advance_requests;
DROP INDEX IF EXISTS idx_users_org;
DROP TABLE IF EXISTS users;
