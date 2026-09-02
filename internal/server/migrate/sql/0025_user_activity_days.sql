-- Dashboard activity: one row per person per UTC calendar day on which they
-- made any authenticated dashboard request, kept forever. login_events
-- alone cannot answer "who is using this": a session lasts seven days, so
-- a daily user who signed in on the 28th shows zero sign-ins for the
-- following month. The session-touch path (rate-limited to one write per
-- session per five minutes) and successful logins upsert today's row.
--
-- Rows are keyed per account (user_id), so a deleted account keeps its own
-- last-active value when the same person is re-provisioned under a fresh
-- uuid the same day. identity is the same stable per-person key as
-- login_events.identity (issuer + subject for SSO, username for local) and
-- is what the active-user counts dedupe on, so one person stays one count
-- across renames, deletion, and JIT re-provisioning. Like login_events,
-- user_id is deliberately NOT a foreign key: the record must outlive its
-- subject.
CREATE TABLE user_activity_days (
    user_id      uuid        NOT NULL,
    identity     text        NOT NULL,
    day          date        NOT NULL,
    -- Last request seen on that day, at the touch cadence's resolution.
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);

-- Day-leading so the 12 monthly count(DISTINCT identity) buckets on the
-- admin Users page are index-only range scans, not table scans, as the
-- forever-retained table grows.
CREATE INDEX user_activity_days_day_idx ON user_activity_days (day, identity);

-- Backfill: every historical sign-in is a lower bound on activity, so the
-- active-users chart is continuous across the upgrade instead of stepping
-- up from zero on deploy day.
INSERT INTO user_activity_days (user_id, identity, day, last_seen_at)
SELECT user_id,
       -- identity is a function of user_id (SSO: immutable issuer+subject;
       -- local: username, which has no rename path).
       min(identity),
       (occurred_at AT TIME ZONE 'UTC')::date,
       max(occurred_at)
  FROM login_events
 GROUP BY user_id, (occurred_at AT TIME ZONE 'UTC')::date;
