import datetime
import unittest

import audit
import db
import retention
import sessions

NOW = datetime.date(2026, 6, 1)


def ago(days):
    return NOW - datetime.timedelta(days=days)


def left(conn, table):
    return conn.execute("SELECT COUNT(*) FROM " + table).fetchone()[0]


class SessionsTest(unittest.TestCase):
    def test_old_sessions_go_and_recent_ones_stay(self):
        conn = db.connect(":memory:")
        sessions.touch(conn, "ana", ago(retention.SESSION_DAYS - 1))
        sessions.touch(conn, "bo", ago(retention.SESSION_DAYS + 1))
        counts = retention.purge(conn, NOW)
        self.assertEqual(counts["sessions"], 1)
        self.assertEqual(left(conn, "sessions"), 1)


class AuditTest(unittest.TestCase):
    def test_audit_rows_are_kept_for_400_days(self):
        conn = db.connect(":memory:")
        audit.record(conn, "ana", "grant", ago(399))
        audit.record(conn, "bo", "revoke", ago(401))
        counts = retention.purge(conn, NOW)
        self.assertEqual(counts["audit"], 1)
        self.assertEqual(left(conn, "audit"), 1)


if __name__ == "__main__":
    unittest.main()
