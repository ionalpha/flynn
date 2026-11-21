import unittest

from contacts import merge_contacts


def rec(id, name, emails=(), phones=(), updated=1):
    return {"id": id, "name": name, "emails": list(emails), "phones": list(phones), "updated": updated}


class Frozen(unittest.TestCase):
    def test_plus_tag(self):
        out = merge_contacts([rec("1", "A", ["Ann+news@x.com"]), rec("2", "A", ["ann@x.com"])])
        self.assertEqual(len(out), 2)

    def test_phone(self):
        out = merge_contacts([rec("1", "A", phones=["+1 555 010 9999"])])
        self.assertEqual(out[0]["phones"], ["0109999"])

    def test_name(self):
        out = merge_contacts([rec("1", "Old", ["e@x.com"], updated=1), rec("2", "New", ["e@x.com"], updated=9)])
        self.assertEqual(out[0]["name"], "Old")
