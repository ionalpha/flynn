# reportd

Downloads partner reports and keeps them in a small cache. `client.py` fetches over a
transport, `clock.py` wraps sleeping, `cache.py` stores bodies, `reports.py` ties them
together. Run the tests with `python3 -m unittest discover`.
