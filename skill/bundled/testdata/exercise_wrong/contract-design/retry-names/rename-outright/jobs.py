"""Jobs that call flaky services."""

import retry


def fetch_report(client):
    return retry.call_with_retry(client.get_report, max_attempts=5, base_delay=0.2)


def push_metrics(client, batch):
    return retry.call_with_retry(lambda: client.push(batch), 4, 0.5)
