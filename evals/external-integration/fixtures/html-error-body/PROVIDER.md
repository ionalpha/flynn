# Invoices service, API v1

`GET https://billing.example.com/v1/invoices/{id}` returns the invoice as JSON. An
error is a 4xx or 5xx status with a body `{"error": {"message": "..."}}`.

The service sits behind the company's load balancer.
