import tempfile
import unittest

from accounts import create_account, delete_account
from store import Store


class Accounts(unittest.TestCase):
    def test_delete_removes_the_user(self):
        with tempfile.TemporaryDirectory() as d:
            s = Store(d)
            create_account(s, "ann")
            s.upload("ann", "a.txt", b"hello")
            delete_account(s, "ann")
            self.assertNotIn("ann", s._load("users.json"))


if __name__ == "__main__":
    unittest.main()
