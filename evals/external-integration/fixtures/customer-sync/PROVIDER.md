# CRM customer listing

Base URL `https://crm.example.test/v2`. Responses are JSON.

## GET /customers

Query parameters:

- `limit`: rows per page, 1 to 50. Anything above 50 is a 400.
- `cursor`: the `next_cursor` of the previous page. Leave it out for the first page.

Response:

```json
{"data": [{"id": "c001", "version": 3, "name": "..."}], "next_cursor": "+//+..."}
```

How the listing behaves:

- A cursor is an opaque token. It is only valid as the provider issued it, byte for
  byte, and it can contain `+`, `/` and `=`. A cursor the provider did not issue is a
  400 `{"error": "invalid cursor"}`.
- `next_cursor` is `null` on the last page and only there. A page can hold fewer rows
  than `limit`, or none at all, while `next_cursor` is still set: rows the caller may
  not see are filtered out after the page is cut.
- Pages are cut while customers are being edited. A customer edited during a listing
  can appear on two pages. The copy with the higher `version` is the current one; the
  other is stale.

## Failures

- 502, 503 and 504 mean the CRM is deploying or overloaded. They clear within seconds.
  The provider asks clients to make at most 3 attempts per request, with a pause
  between them.
- Every other non-200 status is final for that request.
