import sqlite3
import unittest

from db import find_users


class FindUsersTest(unittest.TestCase):
    def setUp(self):
        self.conn = sqlite3.connect(":memory:")
        self.conn.executescript(
            "CREATE TABLE users (id INTEGER, name TEXT, email TEXT);"
            "INSERT INTO users VALUES (1, 'ann', 'ann@x.test'), (2, 'bob', 'bob@x.test'), (3, 'ann', 'ann2@x.test');"
        )

    def test_by_name(self):
        self.assertEqual(find_users(self.conn, "bob"), [(2, "bob")])

    def test_sorted_by_id(self):
        self.assertEqual(find_users(self.conn, "ann", sort="id"), [(1, "ann"), (3, "ann")])


if __name__ == "__main__":
    unittest.main()
