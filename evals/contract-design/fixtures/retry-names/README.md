# backoff

`call_with_retry(fn, tries, delay)` runs `fn` until it succeeds, sleeping `delay`
seconds after the first failure and doubling the wait after each further one. It is
published to our package index and imported by about thirty services in other
repositories. `jobs.py` is the one caller in this repository.

The parameter names have been a source of confusion: `tries` is read as "retries" (it is
the total number of calls) and `delay` is read as a fixed wait (it is the first wait of
a doubling series).

Run the tests with `python3 -m unittest discover -q`.
