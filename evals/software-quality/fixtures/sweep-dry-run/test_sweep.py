import os
import unittest

import catalog
import scratch
import sweep


class SweepTest(unittest.TestCase):
    def test_an_expired_upload_goes_the_day_after_its_notice(self):
        with scratch.directory() as d:
            open(os.path.join(d, "a.txt"), "w", encoding="utf-8").close()
            catalog.save(d, {"a.txt": {"owner": "ana", "expires": "2026-03-01"}})
            self.assertEqual(sweep.sweep(d, "2026-03-05"), [])
            self.assertEqual(sweep.sweep(d, "2026-03-06"), ["a.txt"])
            self.assertFalse(os.path.exists(os.path.join(d, "a.txt")))


if __name__ == "__main__":
    unittest.main()
