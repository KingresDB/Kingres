-- SPDX-License-Identifier: PostgreSQL
--
-- Cluster-wide part of the integration test fixture: roles and databases.
-- Run as the instance superuser by scripts/pg-test-instance.sh. Passwords are
-- throwaway test values for a throwaway instance.

CREATE ROLE kingres_admin NOLOGIN;
CREATE ROLE kingres_ops NOLOGIN;
CREATE ROLE kingres_readonly NOLOGIN;
CREATE ROLE kingres_stats NOLOGIN;

-- The service role of kingresd (the DSN of the tests).
CREATE ROLE cp_service LOGIN PASSWORD 'cp_service_pw';
GRANT pg_monitor TO cp_service;

CREATE ROLE cp_admin LOGIN PASSWORD 'cp_admin_pw' CREATEDB;
GRANT kingres_admin TO cp_admin;

CREATE ROLE cp_ro LOGIN PASSWORD 'cp_ro_pw' CONNECTION LIMIT 7;
GRANT kingres_readonly TO cp_ro;

-- Cluster role through another role (transitive membership).
CREATE ROLE cp_team NOLOGIN;
GRANT kingres_ops TO cp_team;
CREATE ROLE cp_member LOGIN PASSWORD 'cp_member_pw' VALID UNTIL '2099-01-01 00:00:00+00';
GRANT cp_team TO cp_member;

-- Valid PostgreSQL role without any cluster role.
CREATE ROLE cp_none LOGIN PASSWORD 'cp_none_pw';

-- A superuser is NOT automatically kingres_admin.
CREATE ROLE cp_super LOGIN SUPERUSER PASSWORD 'cp_super_pw';

-- A disabled account that is still a member of a cluster role.
CREATE ROLE cp_disabled NOLOGIN;
GRANT kingres_admin TO cp_disabled;

CREATE DATABASE kingres_cp OWNER cp_admin;
CREATE DATABASE plain_cp OWNER cp_admin;
ALTER DATABASE plain_cp SET synchronous_commit = 'remote_apply';
