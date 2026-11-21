import subprocess
import sys
import unittest


class Importer(unittest.TestCase):
    def test_runs_and_reports(self):
        p = subprocess.run([sys.executable, "importer.py", "rows.csv"], capture_output=True, text=True)
        self.assertEqual(p.returncode, 0)
        self.assertIn("imported", p.stdout)


if __name__ == "__main__":
    unittest.main()
