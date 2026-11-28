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


def _refresh(creds):
    res = transport.post(
        BASE + "/oauth/token",
        {"grant_type": "refresh_token", "refresh_token": creds["refresh_token"]},
    )
    if res.status != 200:
        raise RuntimeError("token refresh answered %d" % res.status)
    return json.loads(res.body)


def get_contacts():
    creds = credentials.load()
    res = _get(creds["access_token"])
    if res.status == 401:
        creds = _refresh(creds)
        # The old refresh token is spent the moment this one is issued, so store it first.
        credentials.save(creds)
        res = _get(creds["access_token"])
    return _contacts(res)
