import unittest

import settings


class SettingsTest(unittest.TestCase):
    def test_reads_values(self):
        self.assertEqual(settings.load("service.env")["WORKERS"], "4")


if __name__ == "__main__":
    unittest.main()
