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
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (email, report_date);


-- Реализация с CDC для БД CRM (таблица customers):

CREATE TABLE IF NOT EXISTS dwh.crm_customers_kafka(id Int32, name String, email String, age Float64, gender String, country String, address String, phone String, __lsn UInt64, __deleted String)
ENGINE = Kafka
SETTINGS kafka_broker_list = 'kafka:9092', kafka_topic_list = 'crm.public.customers', kafka_group_name = 'clickhouse', kafka_format = 'JSONEachRow';

CREATE TABLE IF NOT EXISTS dwh.crm_customers(id Int32, name String, email String, age Float64, gender String, country String, address String, phone String, lsn UInt64, is_deleted UInt8)
ENGINE = ReplacingMergeTree(lsn, is_deleted)
ORDER BY id;

CREATE MATERIALIZED VIEW IF NOT EXISTS dwh.crm_customers_mv TO dwh.crm_customers AS
  SELECT id, name, email, age, gender, country, address, phone, __lsn AS lsn, __deleted = 'true' AS is_deleted
  FROM dwh.crm_customers_kafka;

CREATE TABLE IF NOT EXISTS dwh.customer_telemetry_mart(email String, report_date Date, name String, events_count UInt64, avg_value Float64, min_value Float64, max_value Float64, lsn UInt64, is_deleted UInt8)
ENGINE = ReplacingMergeTree(lsn, is_deleted)
ORDER BY (email, report_date);

CREATE MATERIALIZED VIEW IF NOT EXISTS dwh.customer_telemetry_mart_mv TO dwh.customer_telemetry_mart AS
  SELECT c.email AS email, toDate(t.event_time) AS report_date, c.name AS name, count() AS events_count, avg(t.value) AS avg_value, min(t.value) AS min_value, max(t.value) AS max_value, c.lsn AS lsn, c.is_deleted AS is_deleted
  FROM dwh.crm_customers AS c
  INNER JOIN dwh.crm_devices AS d ON d.email = c.email
  INNER JOIN dwh.telemetry AS t ON t.device_id = d.device_id
  GROUP BY c.email, report_date, c.name, c.lsn, c.is_deleted;
