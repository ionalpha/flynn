import json

import credentials
import transport

BASE = "https://crm.example.com"


def _get(token):
    return transport.get(BASE + "/contacts", headers={"Authorization": "Bearer " + token})


def _contacts(res):
    if res.status != 200:
        raise RuntimeError("CRM answered %d" % res.status)
    return json.loads(res.body)["contacts"]


def get_contacts():
    return _contacts(_get(credentials.load()["access_token"]))
