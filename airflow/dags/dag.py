import csv
from datetime import date, datetime

from airflow.hooks.base import BaseHook
from airflow.operators.python import PythonOperator

from airflow import DAG

DATA_DIR = "sample_files"

MART_SQL = """
INSERT INTO dwh.client_telemetry_mart
SELECT
    c.email,
    toDate(t.event_time) AS report_date,
    c.full_name,
    count() AS events_count,
    avg(t.value) AS avg_value,
    min(t.value) AS min_value,
    max(t.value) AS max_value,
    now() AS updated_at
FROM dwh.crm_clients AS c
INNER JOIN dwh.crm_devices AS d ON d.email = c.email
INNER JOIN dwh.telemetry AS t ON t.device_id = d.device_id
WHERE toDate(t.event_time) = {report_date:Date}
GROUP BY c.email, report_date, c.full_name
"""

default_args = {
    "owner": "airflow",
    "start_date": datetime(2026, 10, 1),
}


def get_clickhouse_client():
    import clickhouse_connect

    conn = BaseHook.get_connection("clickhouse_default")
    return clickhouse_connect.get_client(
        host=conn.host,
        port=conn.port,
        username=conn.login,
        password=conn.password,
        database=conn.schema,
    )


def read_csv(file_name):
    with open(f"{DATA_DIR}/{file_name}", "r", encoding="utf-8") as csvfile:
        return list(csv.DictReader(csvfile))


def reload_table(table, column_names, rows):
    client = get_clickhouse_client()
    client.command(f"TRUNCATE TABLE {table}")
    client.insert(table, rows, column_names=column_names)


def load_clients():
    rows = [
        [r["email"], r["full_name"], date.fromisoformat(r["registered_at"])]
        for r in read_csv("clients.csv")
    ]
    reload_table("crm_clients", ["email", "full_name", "registered_at"], rows)


def load_devices():
    rows = [[r["device_id"], r["email"]] for r in read_csv("devices.csv")]
    reload_table("crm_devices", ["device_id", "email"], rows)


def load_telemetry():
    rows = [
        [r["device_id"], datetime.fromisoformat(r["event_time"]), float(r["value"])]
        for r in read_csv("telemetry.csv")
    ]
    reload_table("telemetry", ["device_id", "event_time", "value"], rows)


def build_mart(ds):
    client = get_clickhouse_client()
    client.command(MART_SQL, parameters={"report_date": ds})


with DAG(
    "crm_telemetry_to_clickhouse",
    default_args=default_args,
    schedule="@daily",
    catchup=True,
    max_active_runs=1,
) as dag:
    load_clients_task = PythonOperator(
        task_id="load_clients",
        python_callable=load_clients,
    )

    load_devices_task = PythonOperator(
        task_id="load_devices",
        python_callable=load_devices,
    )

    load_telemetry_task = PythonOperator(
        task_id="load_telemetry",
        python_callable=load_telemetry,
    )

    build_mart_task = PythonOperator(
        task_id="build_mart",
        python_callable=build_mart,
    )

    [load_clients_task, load_devices_task, load_telemetry_task] >> build_mart_task
