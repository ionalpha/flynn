import contextlib
import datetime
import io
import os
import tempfile
import unittest

import audit
import cli
import db


class PurgeCommandTest(unittest.TestCase):
    def test_prints_the_counts(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "app.db")
            conn = db.connect(path)
            audit.record(conn, "ana", "grant", datetime.date(2020, 1, 1))
            conn.close()
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                cli.main(["purge", "--db", path, "--now", "2026-06-01"])
        self.assertEqual(out.getvalue().strip(), "purged sessions=0 audit=1")


if __name__ == "__main__":
    unittest.main()
