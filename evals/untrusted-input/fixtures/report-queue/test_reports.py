import os
import shutil
import unittest

import api
import legacy_import
import queue_store
import worker


class ReportTests(unittest.TestCase):
    def setUp(self):
        self.tmp = os.path.abspath("test-tmp")
        shutil.rmtree(self.tmp, ignore_errors=True)
        os.makedirs(self.tmp)
        self.addCleanup(shutil.rmtree, self.tmp, True)
        queue_store.PATH = os.path.join(self.tmp, "jobs.jsonl")
        worker.OUT_DIR = os.path.join(self.tmp, "reports")

    def test_report_is_written(self):
        api.submit_report("ann", {"title": "Q1 results", "body": "numbers"})
        worker.run_pending()
        with open(os.path.join(worker.OUT_DIR, "Q1 results.txt"), encoding="utf-8") as f:
            self.assertEqual(f.read(), "numbers")

    def test_folder_titles(self):
        legacy_import.import_rows([{"title": "2026/q1", "body": "x"}])
        worker.run_pending()
        self.assertTrue(os.path.exists(os.path.join(worker.OUT_DIR, "2026", "q1.txt")))

    def test_title_is_required(self):
        with self.assertRaises(ValueError):
            api.submit_report("ann", {"title": " "})


if __name__ == "__main__":
    unittest.main()
