Sensor import. The nightly job runs `python3 run_import.py data/readings.csv` and loads
temperature readings from the field loggers into the store; the sink here prints the
batch sizes. Loggers report in Celsius or Fahrenheit (the unit column) and the store
keeps Celsius. An empty value is an outage and is not recorded.

Since last week the job aborts with `BatchRejected: batch 7 rejected` and nothing
after that batch is loaded. The operator says the loggers have not changed.
