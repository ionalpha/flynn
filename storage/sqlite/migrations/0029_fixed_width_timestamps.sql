-- Rewrite every stored timestamp to the zero-padded, fixed-width encoding that
-- sqlitex.TimeLayout now writes.
--
-- The columns below were written with time.RFC3339Nano, which strips trailing
-- zeros from the fractional second. The result is variable-width and does not
-- compare lexicographically: '.' sorts before 'Z', so a reading that lands
-- exactly on a second ("...T00:00:00Z") sorts after every reading in that same
-- second that has a fractional part, the reverse of the truth. Any ORDER BY or
-- range predicate over one of these columns was wrong for those rows.
--
-- A half-migrated table compares worse than a consistently-wrong one, so every
-- affected column is rewritten here in the one transaction the migration runner
-- wraps this file in. Each UPDATE is a pure reformat: it pads the fractional
-- second to nine digits and preserves the instant exactly, so nothing derived
-- from the parsed value changes. In particular the Merkle leaf a checkpoint
-- signs is built from the event's nanosecond integer, not from this string, so
-- rewriting events.time leaves every existing proof valid.
--
-- Re-running is a no-op. The guard skips any value already 30 characters long,
-- which is precisely the padded form: an unpadded value with nine fractional
-- digits has a non-zero last digit, so RFC3339Nano would have left it alone and
-- it is already correct. Values that are not this encoding at all (a NULL, or
-- anything not ending in 'Z' with '.' or 'Z' at position 20) are left untouched
-- rather than mangled.
--
-- The shape of the reformat, for a value BASE(19 chars) [ '.' FRAC ] 'Z':
--   substr(v,1,19)                    the second-precision prefix
--   substr(v,21,length(v)-21)         FRAC, empty when there is no '.'
--   || '000000000', 1, 9              right-pad to nine digits

UPDATE sessions SET
  created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN substr(created_at,20,1) = '.' THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE created_at IS NOT NULL AND length(created_at) BETWEEN 20 AND 29 AND substr(created_at,length(created_at),1) = 'Z' AND substr(created_at,20,1) IN ('.','Z');
UPDATE sessions SET
  updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN substr(updated_at,20,1) = '.' THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE updated_at IS NOT NULL AND length(updated_at) BETWEEN 20 AND 29 AND substr(updated_at,length(updated_at),1) = 'Z' AND substr(updated_at,20,1) IN ('.','Z');

UPDATE turns SET
  created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN substr(created_at,20,1) = '.' THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE created_at IS NOT NULL AND length(created_at) BETWEEN 20 AND 29 AND substr(created_at,length(created_at),1) = 'Z' AND substr(created_at,20,1) IN ('.','Z');

UPDATE skills SET
  created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN substr(created_at,20,1) = '.' THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE created_at IS NOT NULL AND length(created_at) BETWEEN 20 AND 29 AND substr(created_at,length(created_at),1) = 'Z' AND substr(created_at,20,1) IN ('.','Z');
UPDATE skills SET
  updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN substr(updated_at,20,1) = '.' THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE updated_at IS NOT NULL AND length(updated_at) BETWEEN 20 AND 29 AND substr(updated_at,length(updated_at),1) = 'Z' AND substr(updated_at,20,1) IN ('.','Z');

UPDATE memory_items SET
  created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN substr(created_at,20,1) = '.' THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE created_at IS NOT NULL AND length(created_at) BETWEEN 20 AND 29 AND substr(created_at,length(created_at),1) = 'Z' AND substr(created_at,20,1) IN ('.','Z');

UPDATE events SET
  time = substr(time,1,19) || '.' || substr(CASE WHEN substr(time,20,1) = '.' THEN substr(time,21,length(time)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE time IS NOT NULL AND length(time) BETWEEN 20 AND 29 AND substr(time,length(time),1) = 'Z' AND substr(time,20,1) IN ('.','Z');

UPDATE resources SET
  created_at = substr(created_at,1,19) || '.' || substr(CASE WHEN substr(created_at,20,1) = '.' THEN substr(created_at,21,length(created_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE created_at IS NOT NULL AND length(created_at) BETWEEN 20 AND 29 AND substr(created_at,length(created_at),1) = 'Z' AND substr(created_at,20,1) IN ('.','Z');
UPDATE resources SET
  updated_at = substr(updated_at,1,19) || '.' || substr(CASE WHEN substr(updated_at,20,1) = '.' THEN substr(updated_at,21,length(updated_at)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE updated_at IS NOT NULL AND length(updated_at) BETWEEN 20 AND 29 AND substr(updated_at,length(updated_at),1) = 'Z' AND substr(updated_at,20,1) IN ('.','Z');
UPDATE resources SET
  valid_from = substr(valid_from,1,19) || '.' || substr(CASE WHEN substr(valid_from,20,1) = '.' THEN substr(valid_from,21,length(valid_from)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE valid_from IS NOT NULL AND length(valid_from) BETWEEN 20 AND 29 AND substr(valid_from,length(valid_from),1) = 'Z' AND substr(valid_from,20,1) IN ('.','Z');
UPDATE resources SET
  valid_to = substr(valid_to,1,19) || '.' || substr(CASE WHEN substr(valid_to,20,1) = '.' THEN substr(valid_to,21,length(valid_to)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE valid_to IS NOT NULL AND length(valid_to) BETWEEN 20 AND 29 AND substr(valid_to,length(valid_to),1) = 'Z' AND substr(valid_to,20,1) IN ('.','Z');
UPDATE resources SET
  deletion_timestamp = substr(deletion_timestamp,1,19) || '.' || substr(CASE WHEN substr(deletion_timestamp,20,1) = '.' THEN substr(deletion_timestamp,21,length(deletion_timestamp)-21) ELSE '' END || '000000000',1,9) || 'Z'
  WHERE deletion_timestamp IS NOT NULL AND length(deletion_timestamp) BETWEEN 20 AND 29 AND substr(deletion_timestamp,length(deletion_timestamp),1) = 'Z' AND substr(deletion_timestamp,20,1) IN ('.','Z');
