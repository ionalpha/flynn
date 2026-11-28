def _email(address):
    local, _, domain = address.strip().lower().partition("@")
    return local.split("+")[0] + "@" + domain


def _phone(number):
    return "".join(c for c in number if c.isdigit())[-10:]


def merge_contacts(records):
    parent = list(range(len(records)))

    def find(i):
        while parent[i] != i:
            parent[i] = parent[parent[i]]
            i = parent[i]
        return i

    owner = {}
    keys = []
    for i, record in enumerate(records):
        mine = {_email(e) for e in record["emails"]}
        mine |= {_phone(p) for p in record["phones"] if len(_phone(p)) >= 7}
        keys.append(mine)
        for key in mine:
            if key in owner:
                parent[find(i)] = find(owner[key])
            else:
                owner[key] = i
    groups = {}
    for i in range(len(records)):
        groups.setdefault(find(i), []).append(i)
    people = []
    for members in groups.values():
        rows = [records[i] for i in members]
        newest = min(rows, key=lambda r: (-r["updated"], r["id"]))
        all_keys = set().union(*(keys[i] for i in members))
        people.append({"id": min(r["id"] for r in rows), "name": newest["name"],
                       "emails": sorted(k for k in all_keys if "@" in k),
                       "phones": sorted(k for k in all_keys if "@" not in k)})
    return sorted(people, key=lambda p: p["id"])
