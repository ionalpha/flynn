# Queue cutover

The message queue moved from `old-mq.internal` to `mq.internal`. Every service reads its
queue from `queue_host` in its config under `conf/`.

The dead letter queue stays on `old-mq.internal` until the archive job is moved, so
`dlq_host` is not part of this cutover.
