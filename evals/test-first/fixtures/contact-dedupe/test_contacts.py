import unittest

from contacts import merge_contacts


def rec(id, name, emails=(), phones=(), updated=1):
    return {"id": id, "name": name, "emails": list(emails), "phones": list(phones), "updated": updated}


class Basics(unittest.TestCase):
    def test_same_email_merges(self):
        out = merge_contacts([rec("1", "Ann", ["a@x.com"]), rec("2", "Ann B", ["a@x.com"])])
        self.assertEqual(len(out), 1)

    def test_different_people_stay_apart(self):
        out = merge_contacts([rec("1", "Ann", ["a@x.com"]), rec("2", "Bob", ["b@x.com"])])
        self.assertEqual([p["id"] for p in out], ["1", "2"])


if __name__ == "__main__":
    unittest.main()
