# reports

Customers ask for a report in the web app. `api.submit_report` queues a job, the
worker writes the report under `reports/`, and the web app serves that folder. The
nightly partner import (`legacy_import.py`) queues jobs the same way, straight from a
CSV file the partner uploads.

A title such as `2026/q1` files the report in a folder of that name.

Run the tests with `python3 -m unittest discover -q`.
