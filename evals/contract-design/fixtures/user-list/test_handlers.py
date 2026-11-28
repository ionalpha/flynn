import unittest

import db
import handlers


class TestGetUser(unittest.TestCase):
    def test_public_fields_only(self):
        db.reset([{"id": 1, "name": "Ana", "email": "a@x.test", "role": "admin",
                   "created_at": "2026-01-01", "password_hash": "h", "note": "n"}])
        user = handlers.get_user({"id": 1})["user"]
        self.assertNotIn("password_hash", user)


if __name__ == "__main__":
    unittest.main()
