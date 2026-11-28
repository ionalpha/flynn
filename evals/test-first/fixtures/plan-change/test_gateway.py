import unittest

from accounts import Accounts
from gateway import Gateway


class Allow(unittest.TestCase):
    def test_free_user_is_stopped_after_five(self):
        gw = Gateway(Accounts(), lambda: 0)
        self.assertEqual([gw.allow("u") for _ in range(6)], [True] * 5 + [False])

    def test_a_new_window_starts_again(self):
        now = [0]
        gw = Gateway(Accounts(), lambda: now[0])
        for _ in range(5):
            gw.allow("u")
        now[0] = 60
        self.assertTrue(gw.allow("u"))


if __name__ == "__main__":
    unittest.main()
