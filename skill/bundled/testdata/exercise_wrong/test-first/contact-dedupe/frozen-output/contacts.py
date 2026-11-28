"""Contact merging. Two records are one person when they share an email or the last 7 digits of a phone."""


def _email(address):
    return address.strip()


def _phone(number):
    return "".join(c for c in number if c.isdigit())[-7:]


def merge_contacts(records):
    merged = []
    for record in records:
        keys = {_email(e) for e in record["emails"]} | {_phone(p) for p in record["phones"] if _phone(p)}
        for person in merged:
            if keys & person["keys"]:
                person["keys"] |= keys
                person["emails"] |= {_email(e) for e in record["emails"]}
                person["phones"] |= {_phone(p) for p in record["phones"] if _phone(p)}
                break
        else:
            merged.append({"id": record["id"], "name": record["name"], "keys": keys,
                           "emails": {_email(e) for e in record["emails"]},
                           "phones": {_phone(p) for p in record["phones"] if _phone(p)}})
    return [{"id": p["id"], "name": p["name"], "emails": sorted(p["emails"]), "phones": sorted(p["phones"])}
            for p in merged]
