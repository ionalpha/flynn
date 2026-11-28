import json
import re
import sys

sys.path.insert(0, ".")
fails = []
ALLOWED = {"id", "name", "email", "role", "created_at"}


def row(i):
    return {
        "id": i, "name": "User %d" % i, "email": "u%d@example.test" % i, "role": "member",
        "created_at": "2026-01-%02d" % (1 + (i - 1) // 12), "password_hash": "hash-%d" % i,
        "internal_note": "note %d" % i, "billing_ref": "bill-%d" % i,
    }


def unwrap(resp):
    """The page's items and next cursor, however the envelope names the list."""
    if not isinstance(resp, dict):
        return None, None, "the response is a %s, not an object: there is nowhere to put the next field" % type(resp).__name__
    lists = [v for v in resp.values() if isinstance(v, list)]
    if len(lists) != 1:
        return None, None, "the response should carry one list of users, found %d lists" % len(lists)
    if "next_cursor" not in resp:
        return None, None, "the response has no next_cursor"
    return lists[0], resp["next_cursor"], None


try:
    import db
    import handlers

    db.reset([row(i) for i in range(1, 121)])
    seen = []
    page_no = 0
    cursor = None
    inserted = False
    while True:
        params = {} if cursor is None else {"cursor": cursor}
        resp = handlers.list_users(params)
        items, nxt, err = unwrap(resp)
        if err:
            fails.append(err)
            break
        json.dumps(resp)
        page_no += 1
        if page_no == 1 and len(items) != 50:
            fails.append("the default page should hold 50 users, got %d" % len(items))
        for it in items:
            extra = set(it) - ALLOWED
            if extra:
                fails.append("users carry fields nobody promised: %s" % sorted(extra))
            if not {"id", "name", "email"} <= set(it):
                fails.append("a user is missing id, name or email")
        seen += [it.get("id") for it in items]
        if page_no == 1 and not inserted:
            for i in (121, 122, 123):
                r = row(i)
                r["created_at"] = "2026-02-01"
                db.add_user(r)
            inserted = True
        if nxt is None:
            break
        if not isinstance(nxt, str) or re.fullmatch(r"\d+", nxt):
            fails.append("next_cursor %r reads as an offset or a row number; make it opaque" % (nxt,))
            break
        cursor = nxt
        if page_no > 10:
            fails.append("paging never ends")
            break
    original = [i for i in seen if i <= 120]
    if len(original) != len(set(original)):
        dup = sorted({i for i in original if original.count(i) > 1})[:5]
        fails.append("users repeated across pages when new users arrived mid-listing: %s" % dup)
    missing = sorted(set(range(1, 121)) - set(seen))
    if missing:
        fails.append("%d users never appeared in any page, for example %s" % (len(missing), missing[:5]))
    if not fails:
        want = sorted(original, key=lambda i: (row(i)["created_at"], i), reverse=True)
        if original != want:
            fails.append("users are not newest first, ties by id descending")

    db.reset([row(i) for i in range(1, 121)])
    items, nxt, err = unwrap(handlers.list_users({"limit": 10}))
    if err is None and len(items) != 10:
        fails.append("limit=10 returned %d users" % len(items))
    items, nxt, err = unwrap(handlers.list_users({"limit": 5000}))
    if err is None and len(items) > 100:
        fails.append("limit above 100 returned %d users" % len(items))
    db.reset([])
    items, nxt, err = unwrap(handlers.list_users({}))
    if err is None and (items != [] or nxt is not None):
        fails.append("an empty table should give an empty page and a null next_cursor")
    db.reset([row(i) for i in range(1, 121)])
    if handlers.get_user({"id": 3}).get("user", {}).get("id") != 3:
        fails.append("get_user changed")
except Exception as e:
    fails.append("list_users is not usable: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
