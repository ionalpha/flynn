import os
import shutil
import unittest

from accounts import create_account, delete_account
from store import Store


class Accounts(unittest.TestCase):
    def setUp(self):
        self.dir = os.path.abspath("test-tmp")
        shutil.rmtree(self.dir, ignore_errors=True)
        os.makedirs(self.dir)
        self.addCleanup(shutil.rmtree, self.dir, True)

    def test_delete_removes_the_user(self):
        s = Store(self.dir)
        create_account(s, "ann")
        s.upload("ann", "a.txt", b"hello")
        delete_account(s, "ann")
        self.assertNotIn("ann", s._load("users.json"))


if __name__ == "__main__":
    unittest.main()
