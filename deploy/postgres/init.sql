-- The demo uses one Postgres instance with one database per service, which is
-- enough to enforce ownership boundaries without running three servers.
CREATE DATABASE chore_catalog;
CREATE DATABASE chore_booking;
CREATE DATABASE chore_notification;
