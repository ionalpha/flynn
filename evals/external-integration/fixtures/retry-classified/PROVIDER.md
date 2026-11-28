# Reports service, API v2

`GET https://reports.example.com/v2/reports/{id}` returns the report as JSON.

| status | when |
|---|---|
| 200 | the report |
| 401 | the token is wrong or lacks the `reports:read` scope |
| 404 | no report with that id |
| 429 | too many requests; `Retry-After` gives the seconds to wait |
| 503 | the service is deploying or overloaded, usually for under a minute |

`Retry-After` is normally a number of seconds. The CDN in front of the service sometimes
answers 429 itself, and it sends an HTTP date there.

## Usage policy

At most three requests per report: the first and two retries. A client that sends more
is blocked for an hour, which turns a short deploy into a long outage.
