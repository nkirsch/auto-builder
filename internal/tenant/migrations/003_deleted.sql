-- an app can be deleted: deleting while the provisioner tears it down, then deleted
ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check
    CHECK (status IN ('provisioning', 'ready', 'failed', 'deleting', 'deleted'));
