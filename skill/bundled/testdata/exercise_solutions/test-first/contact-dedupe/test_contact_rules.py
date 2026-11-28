import copy
import unittest

from contacts import merge_contacts


def rec(id, name, emails=(), phones=(), updated=1):
    return {"id": id, "name": name, "emails": list(emails), "phones": list(phones), "updated": updated}


def ids(out):
    return [p["id"] for p in out]


class Keys(unittest.TestCase):
    def test_plus_tag_is_ignored(self):
        out = merge_contacts([rec("1", "A", ["Ann+news@x.com"]), rec("2", "A", ["ann@x.com"])])
        self.assertEqual([(p["id"], p["emails"]) for p in out], [("1", ["ann@x.com"])])

    def test_case_and_spaces_are_ignored(self):
        out = merge_contacts([rec("1", "A", ["ANN@X.com "]), rec("2", "A", ["ann@x.com"])])
        self.assertEqual([(p["id"], p["emails"]) for p in out], [("1", ["ann@x.com"])])

    def test_country_code_is_dropped_from_a_phone(self):
        out = merge_contacts([rec("1", "A", phones=["+1 (555) 010-9999"]), rec("2", "A", phones=["555-010-9999"])])
        self.assertEqual([(p["id"], p["phones"]) for p in out], [("1", ["5550109999"])])

    def test_a_short_phone_matches_nothing(self):
        out = merge_contacts([rec("1", "A", phones=["12345"]), rec("2", "B", phones=["12345"])])
        self.assertEqual(ids(out), ["1", "2"])

    def test_a_record_without_keys_is_its_own_person(self):
        out = merge_contacts([rec("1", "A"), rec("2", "B")])
        self.assertEqual(ids(out), ["1", "2"])


class Groups(unittest.TestCase):
    def test_sharing_is_transitive_in_any_order(self):
        out = merge_contacts([rec("1", "A", ["e@x.com"]), rec("3", "C", phones=["555-010-1111"]),
                              rec("2", "B", ["e@x.com"], ["555-010-1111"])])
        self.assertEqual(ids(out), ["1"])

    def test_name_comes_from_the_newest_record(self):
        out = merge_contacts([rec("1", "Old", ["e@x.com"], updated=1), rec("2", "New", ["e@x.com"], updated=9)])
        self.assertEqual([(p["id"], p["name"]) for p in out], [("1", "New")])

    def test_equal_updated_takes_the_smaller_id_whatever_the_input_order(self):
        out = merge_contacts([rec("7", "Late", ["e@x.com"], updated=5), rec("3", "Early", ["e@x.com"], updated=5)])
        self.assertEqual([(p["id"], p["name"]) for p in out], [("3", "Early")])

    def test_emails_and_phones_are_sorted(self):
        out = merge_contacts([rec("1", "A", ["z@x.com", "a@x.com"], ["555-010-2222", "555-010-1111"])])
        self.assertEqual((out[0]["emails"], out[0]["phones"]), (["a@x.com", "z@x.com"], ["5550101111", "5550102222"]))

    def test_people_are_ordered_by_id(self):
        out = merge_contacts([rec("b", "B", ["b@x.com"]), rec("a", "A", ["a@x.com"])])
        self.assertEqual(ids(out), ["a", "b"])

    def test_the_input_is_left_alone(self):
        records = [rec("b", "B", ["B@x.com"]), rec("a", "A", ["b@x.com"])]
        before = copy.deepcopy(records)
        merge_contacts(records)
        self.assertEqual(records, before)


if __name__ == "__main__":
    unittest.main()
