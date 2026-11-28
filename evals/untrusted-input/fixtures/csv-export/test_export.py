import unittest

import export
import feedback


class ExportTest(unittest.TestCase):
    def test_every_name_is_listed(self):
        out = export.to_csv(feedback.ROWS)
        for row in feedback.ROWS:
            self.assertIn(row["name"], out)


if __name__ == "__main__":
    unittest.main()
