import unittest

import api
import jobs


class TestJobs(unittest.TestCase):
    def setUp(self):
        del jobs.QUEUE[:]

    def test_submit(self):
        self.assertEqual(api.submit_job({"queue": "mail"}), 1)
        self.assertEqual(jobs.QUEUE[0].priority, 5)

    def test_bad_priority(self):
        with self.assertRaises(ValueError):
            jobs.JobConfig(queue="mail", priority=12).validate()


if __name__ == "__main__":
    unittest.main()
