-- Older versions treated cleanup's cancelled SMTP transactions as notices and left them in the queue.
UPDATE events SET kind = 'cancelled', status = 'cancelled'
WHERE kind = 'notice' AND program LIKE 'postfix%/cleanup' AND raw LIKE '%: removed (canceled)';

UPDATE messages SET
    removed_at = (SELECT max(at) FROM events WHERE message_id = messages.id AND kind = 'cancelled'),
    status = 'cancelled'
WHERE status = 'queued' AND removed_at IS NULL
    AND EXISTS (SELECT 1 FROM events WHERE message_id = messages.id AND kind = 'cancelled')
    AND NOT EXISTS (SELECT 1 FROM recipients WHERE message_id = messages.id);

-- Keep accepted-message counts covered by indexes when subtracting cancelled transactions.
CREATE INDEX messages_cancelled ON messages (received_at, queue_id)
WHERE status = 'cancelled' AND queue_id IS NOT NULL;
