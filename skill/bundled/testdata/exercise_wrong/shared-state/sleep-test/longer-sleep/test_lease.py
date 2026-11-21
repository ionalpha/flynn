import time
import unittest

from lease import Lease


class TestLease(unittest.TestCase):
    def test_expires(self):
        lease = Lease(0.2)
        self.assertFalse(lease.expired())
        time.sleep(0.5)
        self.assertTrue(lease.expired())

    def test_renew_extends(self):
        lease = Lease(2)
        time.sleep(0.3)
        lease.renew()
        time.sleep(0.3)
        self.assertFalse(lease.expired())


if __name__ == "__main__":
    unittest.main()
