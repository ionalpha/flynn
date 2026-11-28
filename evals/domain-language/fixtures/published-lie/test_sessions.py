import unittest

import db
import sessions


class TestSessions(unittest.TestCase):
    def setUp(self):
        db.seed()

    def test_purge_old_sessions_leaves_recent_ones(self):
        sessions.purge_old_sessions("2026-03-01")
        self.assertEqual(sessions.active_sessions(), ["s2", "s4"])

    def test_active_sessions_in_creation_order(self):
        self.assertEqual(sessions.active_sessions(), ["s3", "s1", "s2", "s4"])


if __name__ == "__main__":
    unittest.main()
