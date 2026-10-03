-- Live logs (Faz 5). Payloads carry only the deployment id: NOTIFY payloads
-- are limited to 8000 bytes, and listeners re-read new rows by id anyway.
CREATE FUNCTION notify_deployment_log() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('deployment_logs', NEW.deployment_id::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deployment_logs_notify
    AFTER INSERT ON deployment_logs
    FOR EACH ROW EXECUTE FUNCTION notify_deployment_log();

CREATE FUNCTION notify_deployment_status() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('deployment_status', NEW.id::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deployments_status_notify
    AFTER UPDATE OF status ON deployments
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION notify_deployment_status();
