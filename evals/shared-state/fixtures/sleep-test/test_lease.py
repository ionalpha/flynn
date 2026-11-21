import time
import unittest

from lease import Lease


class TestLease(unittest.TestCase):
    def test_expires(self):
        lease = Lease(1)
        self.assertFalse(lease.expired())
        time.sleep(1.1)
        self.assertTrue(lease.expired())

    def test_renew_extends(self):
        lease = Lease(1)
        time.sleep(0.6)
        lease.renew()
        time.sleep(0.6)
        self.assertFalse(lease.expired())


if __name__ == "__main__":
    unittest.main()
