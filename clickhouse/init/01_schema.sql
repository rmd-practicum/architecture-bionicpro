CREATE DATABASE IF NOT EXISTS dwh;

CREATE TABLE IF NOT EXISTS dwh.crm_clients(email String, full_name String, registered_at Date)
ENGINE = MergeTree
ORDER BY email;

CREATE TABLE IF NOT EXISTS dwh.crm_devices(device_id String, email String)
ENGINE = MergeTree
ORDER BY device_id;

CREATE TABLE IF NOT EXISTS dwh.telemetry(device_id String, event_time DateTime, value Float64)
ENGINE = MergeTree
ORDER BY (device_id, event_time);

CREATE TABLE IF NOT EXISTS dwh.client_telemetry_mart(email String, report_date Date, full_name String, events_count UInt64, avg_value Float64, min_value Float64, max_value Float64, updated_at DateTime)
ENGINE = MergeTree
ORDER BY (email, report_date);
