import unittest

from lease import Lease


class TestLease(unittest.TestCase):
    def test_fresh_lease_is_live(self):
        self.assertFalse(Lease(60).expired())


if __name__ == "__main__":
    unittest.main()
