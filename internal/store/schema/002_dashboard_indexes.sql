-- Indexes the dashboard's counts read on their own, without visiting a row per message: at 200,000 messages
-- in 30 days each of those counts took 120 to 440 ms (internal/store/bench_test.go). SQLite treats a partial
-- index as covering only when it holds the columns of its own WHERE too, hence queue_id and kind.

-- Received: messages with a queue ID in the period.
CREATE INDEX messages_received ON messages (received_at, queue_id) WHERE queue_id IS NOT NULL;

-- Outcomes and delivery delays, by when each recipient's outcome was final.
DROP INDEX recipients_final_at;
CREATE INDEX recipients_final_at ON recipients (final_at, status, delay, message_id) WHERE final_at IS NOT NULL;

-- Deferrals and the outcomes chart.
DROP INDEX events_outcomes;
CREATE INDEX events_outcomes ON events (at, kind, status, message_id) WHERE kind IN ('delivery', 'reject', 'expired');
