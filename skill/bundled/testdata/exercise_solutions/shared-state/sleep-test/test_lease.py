import unittest

from lease import Lease


class FakeClock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now


class TestLease(unittest.TestCase):
    def test_expires_at_ttl(self):
        clock = FakeClock()
        lease = Lease(1, clock)
        clock.now = 0.999
        self.assertFalse(lease.expired())
        clock.now = 1.0
        self.assertTrue(lease.expired())

    def test_renew_extends(self):
        clock = FakeClock()
        lease = Lease(1, clock)
        clock.now = 0.6
        lease.renew()
        clock.now = 1.2
        self.assertFalse(lease.expired())
        clock.now = 1.6
        self.assertTrue(lease.expired())


if __name__ == "__main__":
    unittest.main()
