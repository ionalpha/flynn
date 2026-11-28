# Access log summary

`summary.summarise(path)` reads one day of access log and returns, per endpoint, the
request count, the number of 5xx responses and the 95th percentile latency in
milliseconds. `python3 run_summary.py data/sample.log` prints it.

The container this runs in has a 512 MB limit. The job is killed on the busiest days,
when the log is a few million lines.
