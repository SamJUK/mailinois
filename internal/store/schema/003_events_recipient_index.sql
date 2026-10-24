-- events.recipient_id cascades from recipients, and nothing indexed it: each recipient a purge deleted made the
-- database scan every event. Purging one day of 200,000 messages in 30 days took over 20 minutes and held the
-- write lock throughout (BenchmarkPurge); PostgreSQL doesn't index foreign keys by itself either.
CREATE INDEX events_recipient ON events (recipient_id);
