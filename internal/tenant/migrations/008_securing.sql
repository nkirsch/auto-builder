-- securing: provisioned, but the app's domain or certificate isn't working yet; runs allowed
ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check
    CHECK (status IN ('provisioning', 'securing', 'ready', 'failed', 'deleting', 'deleted'));
