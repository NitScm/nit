-- Tell a waiting client that its task moved, instead of making it ask.
--
-- `GET /v1/tasks/{id}/events` holds a long poll open and re-reads the row every
-- EventPollInterval (500 ms). That is one query per waiting developer per half
-- second, for a row that changes twice in a task's life — and the wait it
-- covers is a developer watching their own push, so the latency is felt.
--
-- A trigger rather than a pg_notify() in the application: this fires for every
-- path that changes a task, including one nobody has written yet and one an
-- operator runs by hand. Notification logic in the store would have to be
-- repeated in Claim, Complete, Fail, Cancel and ReleaseExpired, and forgetting
-- one produces a client that hangs for the full poll interval on exactly one
-- transition.
--
-- NOTIFY is delivered at commit, which is what makes it safe: a listener is
-- never woken to read a row that has not landed.
--
-- The payload is the task id. It is a UUID, far inside the 8000-byte limit that
-- would otherwise make NOTIFY fail at commit time.
--
-- **A dropped notification must not strand a client.** The listening connection
-- can fail and reconnect across a change, so the poll stays as the liveness
-- guarantee — at a much lower rate. See pkg/store.TaskNotifier.

CREATE OR REPLACE FUNCTION notify_task_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('nit_task_changed', NEW.id::text);
    RETURN NULL;
END;
$$;

-- AFTER, so the row is visible to whoever the notification wakes; and only when
-- the state actually moved, because a heartbeat updates lease_expires_at every
-- few seconds and waking every waiter for that would cost more than the poll it
-- replaces.
CREATE TRIGGER task_changed
    AFTER UPDATE OF state ON tasks
    FOR EACH ROW
    WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION notify_task_changed();
