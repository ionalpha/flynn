import os
import unittest

import scratch
import settings


class SettingsTest(unittest.TestCase):
    def test_the_environment_wins_over_the_file(self):
        with scratch.directory() as d:
            path = os.path.join(d, "sync.ini")
            with open(path, "w", encoding="utf-8") as f:
                f.write("[sync]\nretries = 5\n")
            self.assertEqual(settings.load(path, {})["retries"], "5")
            self.assertEqual(settings.load(path, {"SYNC_RETRIES": "9"})["retries"], "9")


if __name__ == "__main__":
    unittest.main()
