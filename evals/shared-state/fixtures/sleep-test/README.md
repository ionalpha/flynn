# leases

A worker takes a Lease on a job and must renew it before ttl seconds pass, or
another worker takes the job. Run the tests with `python3 -m unittest discover -q`.
