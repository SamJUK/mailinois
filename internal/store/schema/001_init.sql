-- Written once for SQLite and PostgreSQL: {{id}} and {{blob}} are swapped for each, and times are
-- microseconds since 1970 (UTC) in BIGINT columns, so the same SQL does arithmetic on them in both.

-- Every Postfix instance that has sent a line, named by its syslog hostname.
CREATE TABLE servers (
    id         {{id}},
    name       TEXT NOT NULL UNIQUE,
    first_seen BIGINT NOT NULL,
    last_seen  BIGINT NOT NULL
);

-- One accepted message per queue ID (queue IDs are reused, so not unique), or one rejected attempt with no
-- queue ID. Status follows from the recipients: deferred > queued > expired > bounced > deleted > rejected > sent.
-- sender_domain is the sender's domain, or the recipient's for bounces (null sender): what sites match on.
CREATE TABLE messages (
    id            {{id}},
    server_id     BIGINT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    queue_id      TEXT,
    received_at   BIGINT NOT NULL,
    client_host   TEXT,
    client_ip     TEXT,
    sasl_username TEXT,
    sender        TEXT,
    sender_domain TEXT,
    message_id    TEXT,
    subject       TEXT,
    size          BIGINT,
    nrcpt         BIGINT,
    status        TEXT NOT NULL DEFAULT 'queued',
    removed_at    BIGINT
);
CREATE INDEX messages_queue_id ON messages (server_id, queue_id, id);
CREATE INDEX messages_received_at ON messages (received_at);
CREATE INDEX messages_sender_domain ON messages (sender_domain, received_at);
CREATE INDEX messages_message_id ON messages (message_id);
CREATE INDEX messages_in_queue ON messages (received_at) WHERE removed_at IS NULL AND queue_id IS NOT NULL;

-- One row per recipient, carrying its latest attempt; every attempt is also an event.
CREATE TABLE recipients (
    id               {{id}},
    message_id       BIGINT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    address          TEXT NOT NULL,
    orig_to          TEXT,
    domain           TEXT NOT NULL,
    provider         TEXT NOT NULL,
    status           TEXT NOT NULL,
    dsn              TEXT,
    relay            TEXT,
    relay_ip         TEXT,
    response         TEXT,
    attempts         BIGINT NOT NULL DEFAULT 0,
    delay            DOUBLE PRECISION,
    first_attempt_at BIGINT,
    last_attempt_at  BIGINT,
    final_at         BIGINT,
    UNIQUE (message_id, address)
);
CREATE INDEX recipients_address ON recipients (address);
CREATE INDEX recipients_domain ON recipients (domain);
CREATE INDEX recipients_final_at ON recipients (final_at) WHERE final_at IS NOT NULL;

-- The log lines themselves, for the timeline, and what was read from them for counting. The same line twice
-- (a re-import, an overlapping docker logs dump) is skipped: line_hash is FNV-1a of raw.
CREATE TABLE events (
    id            {{id}},
    server_id     BIGINT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    message_id    BIGINT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    recipient_id  BIGINT REFERENCES recipients (id) ON DELETE CASCADE,
    at            BIGINT NOT NULL,
    kind          TEXT NOT NULL,
    program       TEXT NOT NULL,
    status        TEXT,
    dsn           TEXT,
    response      TEXT,
    raw           TEXT NOT NULL,
    line_hash     BIGINT NOT NULL,
    recipient_key BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX events_message ON events (message_id, at);
CREATE INDEX events_outcomes ON events (at) WHERE kind IN ('delivery', 'reject', 'expired');
CREATE UNIQUE INDEX events_line ON events (server_id, at, line_hash, recipient_key);

CREATE TABLE captures (
    id         {{id}},
    queue_id   TEXT NOT NULL,
    message_id TEXT,
    server     TEXT,
    at         BIGINT NOT NULL,
    subject    TEXT,
    header     {{blob}} NOT NULL, -- gzip
    body       {{blob}},          -- gzip; full mode only
    size       BIGINT NOT NULL,
    truncated  BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX captures_queue_id ON captures (queue_id, at);
CREATE INDEX captures_at ON captures (at);

CREATE TABLE sending_ips (
    ip                TEXT PRIMARY KEY,
    ptr               TEXT,
    forward_confirmed BIGINT NOT NULL DEFAULT 0,
    checked_at        BIGINT NOT NULL
);

CREATE TABLE dnsbl_checks (
    id         {{id}},
    ip         TEXT NOT NULL,
    zone       TEXT NOT NULL,
    status     TEXT NOT NULL, -- clean, listed, error
    response   TEXT,
    checked_at BIGINT NOT NULL
);
CREATE INDEX dnsbl_checks_latest ON dnsbl_checks (ip, zone, checked_at);

CREATE TABLE dns_checks (
    id         {{id}},
    domain     TEXT NOT NULL,
    kind       TEXT NOT NULL, -- spf, dkim, dmarc
    subject    TEXT NOT NULL, -- spf: the IP; dkim: the selector
    status     TEXT NOT NULL, -- pass, warn, fail, missing, error
    summary    TEXT NOT NULL,
    record     TEXT,
    checked_at BIGINT NOT NULL
);
CREATE INDEX dns_checks_latest ON dns_checks (domain, kind, subject, checked_at);

-- DKIM selectors seen signing, from OpenDKIM's log lines and from captures.
CREATE TABLE dkim_selectors (
    domain    TEXT NOT NULL,
    selector  TEXT NOT NULL,
    last_seen BIGINT NOT NULL,
    PRIMARY KEY (domain, selector)
);

CREATE TABLE alert_states (
    name        TEXT PRIMARY KEY,
    state       TEXT NOT NULL, -- ok, firing
    value       DOUBLE PRECISION,
    since       BIGINT NOT NULL,
    notified_at BIGINT
);

CREATE TABLE alert_events (
    id    {{id}},
    name  TEXT NOT NULL,
    state TEXT NOT NULL,
    value DOUBLE PRECISION,
    at    BIGINT NOT NULL
);
CREATE INDEX alert_events_at ON alert_events (at);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
